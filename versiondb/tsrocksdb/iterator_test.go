package tsrocksdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIteratorDomainNilBounds verifies Domain() returns (nil, nil) when the
// caller passed nil start/end, mirroring cosmos-sdk's prefix.prefixIterator
// convention (see store/v2/prefix.Domain) rather than leaking the internal
// prefix-derived bounds used to scope the underlying rocksdb iterator.
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

// TestIteratorDomainExplicitBounds verifies Domain() returns the caller's
// original, unprefixed start/end -- exactly what Key() strips its results
// down to -- rather than the store-prefixed bounds used internally.
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

// TestIteratorDomainMixedBounds verifies a nil start with an explicit end
// (and vice versa) round-trip correctly, since iterateWithPrefix computes a
// prefix-derived end bound when the caller passes end=nil, which must not be
// mistaken for an explicit bound when reconstructing Domain().
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
