package memiavl

import (
	"runtime/debug"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProofsEmptyTree(t *testing.T) {
	tree := New(0)

	proof, err := tree.GetMembershipProof([]byte("hello"))
	require.Error(t, err)
	require.Nil(t, proof)

	nonExistProof, err := tree.GetNonMembershipProof([]byte("hello"))
	require.Error(t, err)
	require.Nil(t, nonExistProof)
}

func TestProofOutlivesSnapshotWhenZeroCopyDisabled(t *testing.T) {
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

	existKey := []byte("hello1")
	exist, err := ptree.GetMembershipProof(existKey)
	require.NoError(t, err)
	require.NotEmpty(t, exist.GetExist().Path, "test key must have a non-trivial inner path")

	nonExist, err := ptree.GetNonMembershipProof([]byte("hello12"))
	require.NoError(t, err)
	left := nonExist.GetNonexist().Left
	right := nonExist.GetNonexist().Right
	require.NotNil(t, left)
	require.NotNil(t, right)

	require.NoError(t, ptree.Close())

	// turn a fault from touching the unmapped snapshot into a failing panic instead of a crash
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	require.Equal(t, existKey, exist.GetExist().Key)
	require.Equal(t, []byte("world1"), exist.GetExist().Value)
	require.Equal(t, existKey, left.Key)
	require.Equal(t, []byte("world1"), left.Value)
	require.Equal(t, []byte("hello2"), right.Key)
	require.Equal(t, []byte("world1"), right.Value)
}

func TestProofs(t *testing.T) {
	// do a round test for each version in ChangeSets
	testCases := []struct {
		existKey    []byte
		nonExistKey []byte
	}{
		{[]byte("hello"), []byte("hello1")},
		{[]byte("hello1"), []byte("hello2")},
		{[]byte("hello2"), []byte("hell")},
		{[]byte("hello00"), []byte("hell")},
		{[]byte("hello00"), []byte("hello")},
		{[]byte("aello00"), []byte("hello")},
		{[]byte("hello1"), []byte("aello00")},
	}

	tmpDir := t.TempDir()
	tree := New(0)

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			changes := ChangeSets[i]
			tree.ApplyChangeSet(changes)
			_, _, err := tree.SaveVersion(true)
			require.NoError(t, err)

			proof, err := tree.GetMembershipProof(tc.existKey)
			require.NoError(t, err)
			require.True(t, tree.VerifyMembership(proof, tc.existKey))

			proof, err = tree.GetNonMembershipProof(tc.nonExistKey)
			require.NoError(t, err)
			require.True(t, tree.VerifyNonMembership(proof, tc.nonExistKey))

			// test persisted tree
			require.NoError(t, tree.WriteSnapshot(tmpDir))
			snapshot, err := OpenSnapshot(tmpDir)
			require.NoError(t, err)
			ptree := NewFromSnapshot(snapshot, true, 0)
			defer ptree.Close()

			proof, err = ptree.GetMembershipProof(tc.existKey)
			require.NoError(t, err)
			require.True(t, ptree.VerifyMembership(proof, tc.existKey))

			proof, err = ptree.GetNonMembershipProof(tc.nonExistKey)
			require.NoError(t, err)
			require.True(t, ptree.VerifyNonMembership(proof, tc.nonExistKey))
		})
	}
}
