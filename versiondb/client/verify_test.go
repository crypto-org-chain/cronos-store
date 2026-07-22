package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmos/iavl"
	"github.com/stretchr/testify/require"

	"github.com/crypto-org-chain/cronos-store/memiavl"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
)

func TestBuildCommitInfoUsesVersionParam(t *testing.T) {
	storeInfos := []storetypes.StoreInfo{
		{Name: "b", CommitId: storetypes.CommitID{Version: 10}},
		{Name: "a", CommitId: storetypes.CommitID{Version: 7}}, // sorts first, older
	}

	ci := buildCommitInfo(storeInfos, 10)

	require.Equal(t, int64(10), ci.Version, "must use the passed version, not storeInfos[0]'s")
	require.Equal(t, "a", ci.StoreInfos[0].Name, "storeInfos should be sorted by name")
}

// writeStoreChangeSet writes a single change set file for `store` containing entries only for the
// given versions (mimicking a real dump, where a store's changeset file only records the versions
// where that specific store changed).
func writeStoreChangeSet(t *testing.T, changeSetDir, store string, versions []int64) {
	t.Helper()

	storeDir := filepath.Join(changeSetDir, store)
	require.NoError(t, os.MkdirAll(storeDir, os.ModePerm))

	fp, err := os.Create(filepath.Join(storeDir, "block-0"))
	require.NoError(t, err)
	defer fp.Close()

	for _, v := range versions {
		cs := &iavl.ChangeSet{Pairs: []*iavl.KVPair{
			{Key: []byte("key"), Value: []byte("value")},
		}}
		require.NoError(t, WriteChangeSet(fp, v, cs))
	}
}

// TestVerifyOneStoreBumpsVersionOnGaps reproduces the version skew bug: the live path
// (memiavl.MultiTree.SaveVersion) bumps every store's tree version on every block, regardless of
// whether that store had any writes. verifyOneStore must match that behavior instead of only
// bumping the version on versions where this store's changeset happens to have an entry.
func TestVerifyOneStoreBumpsVersionOnGaps(t *testing.T) {
	dir := t.TempDir()

	// "foo" only changed at versions 1 and 3, skipping version 2 entirely - as would happen for
	// a store that had no writes in block 2.
	writeStoreChangeSet(t, dir, "foo", []int64{1, 3})

	tree := memiavl.New(0)
	storeInfo, err := verifyOneStore(tree, "foo", dir, "", 3)
	require.NoError(t, err)
	require.NotNil(t, storeInfo)

	// without the fix, the tree would only be bumped to version 2 (one SaveVersion call per
	// changeset entry) and IterateChangeSets would fail with "version don't match: 2 != 3".
	require.Equal(t, int64(3), tree.Version())
	require.Equal(t, int64(3), storeInfo.CommitId.Version)
}

// TestVerifyOneStoreCatchesUpToTargetVersion checks that a store with no further changesets still
// gets its tree version bumped up to the target version, matching every other store that keeps
// advancing on every block in the live path.
func TestVerifyOneStoreCatchesUpToTargetVersion(t *testing.T) {
	dir := t.TempDir()

	writeStoreChangeSet(t, dir, "foo", []int64{1})

	tree := memiavl.New(0)
	storeInfo, err := verifyOneStore(tree, "foo", dir, "", 5)
	require.NoError(t, err)
	require.NotNil(t, storeInfo)

	require.Equal(t, int64(5), tree.Version())
	require.Equal(t, int64(5), storeInfo.CommitId.Version)
}
