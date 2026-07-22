package tsrocksdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIteratorDomainNilBounds(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, store.PutAtVersion(1, nil))

	itr, err := store.IteratorAtVersion(testStoreKey, nil, nil, nil)
	require.NoError(t, err)
	defer itr.Close()

	start, end := itr.Domain()
	require.Nil(t, start)
	require.Nil(t, end)
}

func TestIteratorDomainExplicitBounds(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	explicitStart := []byte("aaa")
	explicitEnd := []byte("zzz")

	itr, err := store.IteratorAtVersion(testStoreKey, explicitStart, explicitEnd, nil)
	require.NoError(t, err)
	defer itr.Close()

	start, end := itr.Domain()
	require.Equal(t, explicitStart, start)
	require.Equal(t, explicitEnd, end)
}

func TestIteratorDomainMixedBounds(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	explicitEnd := []byte("zzz")
	itr, err := store.IteratorAtVersion(testStoreKey, nil, explicitEnd, nil)
	require.NoError(t, err)
	start, end := itr.Domain()
	require.Nil(t, start)
	require.Equal(t, explicitEnd, end)
	require.NoError(t, itr.Close())

	explicitStart := []byte("aaa")
	itr2, err := store.IteratorAtVersion(testStoreKey, explicitStart, nil, nil)
	require.NoError(t, err)
	start, end = itr2.Domain()
	require.Equal(t, explicitStart, start)
	require.Nil(t, end)
	require.NoError(t, itr2.Close())
}
