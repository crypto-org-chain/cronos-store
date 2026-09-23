package memiavl

import (
	"bytes"
	"runtime/debug"
	"testing"
	"unsafe"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/stretchr/testify/require"
)

func TestIterator(t *testing.T) {
	tree := New(0)
	require.Equal(t, ExpectItems[0], collectIter(tree.Iterator(nil, nil, true)))

	for _, changes := range ChangeSets {
		tree.ApplyChangeSet(changes)
		_, v, err := tree.SaveVersion(true)
		require.NoError(t, err)
		require.Equal(t, ExpectItems[v], collectIter(tree.Iterator(nil, nil, true)))
		require.Equal(t, reverse(ExpectItems[v]), collectIter(tree.Iterator(nil, nil, false)))
	}
}

func TestIteratorRange(t *testing.T) {
	tree := New(0)
	for _, changes := range ChangeSets[:6] {
		tree.ApplyChangeSet(changes)
		_, _, err := tree.SaveVersion(true)
		require.NoError(t, err)
	}

	expItems := []pair{
		{[]byte("aello05"), []byte("world1")},
		{[]byte("aello06"), []byte("world1")},
		{[]byte("aello07"), []byte("world1")},
		{[]byte("aello08"), []byte("world1")},
		{[]byte("aello09"), []byte("world1")},
	}
	require.Equal(t, expItems, collectIter(tree.Iterator([]byte("aello05"), []byte("aello10"), true)))
	require.Equal(t, reverse(expItems), collectIter(tree.Iterator([]byte("aello05"), []byte("aello10"), false)))
}

func TestIteratorZeroCopyDisabledClonesPerPosition(t *testing.T) {
	tmpDir := t.TempDir()
	tree := New(0)
	for _, changes := range ChangeSets[:6] {
		tree.ApplyChangeSet(changes)
		_, _, err := tree.SaveVersion(true)
		require.NoError(t, err)
	}
	require.NoError(t, tree.WriteSnapshot(tmpDir))

	snapshot, err := OpenSnapshot(tmpDir)
	require.NoError(t, err)
	ptree := NewFromSnapshot(snapshot, false, 0)

	iter := ptree.Iterator(nil, nil, true)
	require.True(t, iter.Valid())

	// repeated calls at one position reuse the same clone rather than allocating again
	require.Same(t, unsafe.SliceData(iter.Key()), unsafe.SliceData(iter.Key()))
	require.Same(t, unsafe.SliceData(iter.Value()), unsafe.SliceData(iter.Value()))

	retained := pair{key: iter.Key(), value: iter.Value()}
	want := pair{key: bytes.Clone(retained.key), value: bytes.Clone(retained.value)}

	iter.Next()
	require.True(t, iter.Valid())
	// advancing must hand out a fresh clone, not overwrite what the caller already holds
	require.NotSame(t, unsafe.SliceData(retained.key), unsafe.SliceData(iter.Key()))
	require.NotSame(t, unsafe.SliceData(retained.value), unsafe.SliceData(iter.Value()))

	collected := collectIter(iter)
	require.NoError(t, iter.Close())
	require.NoError(t, ptree.Close())

	// a clone still aliasing the unmapped snapshot faults; make that a failing panic
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	require.Equal(t, want, retained)
	require.Equal(t, ExpectItems[6][1:], collected)
}

type pair struct {
	key, value []byte
}

func collectIter(iter dbm.Iterator) []pair {
	result := []pair{}
	for ; iter.Valid(); iter.Next() {
		result = append(result, pair{key: iter.Key(), value: iter.Value()})
	}
	return result
}

func reverse[S ~[]E, E any](s S) S {
	r := make(S, len(s))
	for i, j := 0, len(s)-1; i <= j; i, j = i+1, j-1 {
		r[i], r[j] = s[j], s[i]
	}
	return r
}
