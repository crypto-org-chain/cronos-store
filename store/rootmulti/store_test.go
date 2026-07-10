package rootmulti

import (
	"bytes"
	"io"
	"testing"

	protoio "github.com/cosmos/gogoproto/io"
	"github.com/stretchr/testify/require"

	log "cosmossdk.io/log/v2"

	snapshottypes "github.com/cosmos/cosmos-sdk/store/v2/snapshots/types"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
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

func TestRestoreRejectsIAVLNodeBeforeStore(t *testing.T) {
	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)

	var buf bytes.Buffer
	w := protoio.NewDelimitedWriter(&buf)
	require.NoError(t, w.WriteMsg(&snapshottypes.SnapshotItem{
		Item: &snapshottypes.SnapshotItem_IAVL{
			IAVL: &snapshottypes.SnapshotIAVLItem{Key: []byte("k"), Value: []byte("v"), Height: 0, Version: 1},
		},
	}))
	require.NoError(t, w.Close())

	r := protoio.NewDelimitedReader(&buf, 1<<20)
	defer r.Close()

	require.NotPanics(t, func() {
		_, err := rs.restore(1, 1, r)
		require.ErrorContains(t, err, "received node item before tree item")
	})
}
