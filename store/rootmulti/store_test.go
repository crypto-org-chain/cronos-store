package rootmulti

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"cosmossdk.io/store/types"
)

const TestAppChainID = "test_chain"

func TestLastCommitID(t *testing.T) {
	store := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	require.Equal(t, types.CommitID{}, store.LastCommitID())
}

func TestCacheMultiStoreWithVersionCloser(t *testing.T) {
	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)

	key := types.NewKVStoreKey("test")
	rs.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	require.NoError(t, rs.LoadLatestVersion())
	t.Cleanup(func() { rs.Close() })

	// Commit version 1 with a key/value.
	kvStore := rs.GetKVStore(key)
	kvStore.Set([]byte("k"), []byte("v"))
	commitID := rs.Commit()
	require.Equal(t, int64(1), commitID.Version)

	// Commit version 2 so that CacheMultiStoreWithVersion(1) must load a
	// separate read-only memiavl DB rather than returning the live CacheMultiStore.
	kvStore = rs.GetKVStore(key)
	kvStore.Set([]byte("k2"), []byte("v2"))
	commitID = rs.Commit()
	require.Equal(t, int64(2), commitID.Version)

	cms, err := rs.CacheMultiStoreWithVersion(1)
	require.NoError(t, err)

	closer, ok := cms.(io.Closer)
	require.True(t, ok, "CacheMultiStoreWithVersion must return an io.Closer")

	val := cms.GetKVStore(key).Get([]byte("k"))
	require.Equal(t, []byte("v"), val)

	require.NoError(t, closer.Close())
}
