package client

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alitto/pond"
	"github.com/cosmos/iavl"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/stretchr/testify/require"

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

func writeStoreChangeSet(t *testing.T, changeSetDir, store string, versions []int64) {
	t.Helper()

	storeDir := filepath.Join(changeSetDir, store)
	require.NoError(t, os.MkdirAll(storeDir, os.ModePerm))

	fp, err := os.Create(filepath.Join(storeDir, fmt.Sprintf("block-%d", versions[0])))
	require.NoError(t, err)
	defer fp.Close()

	for _, v := range versions {
		cs := &iavl.ChangeSet{Pairs: []*iavl.KVPair{
			{Key: []byte("key"), Value: []byte("value")},
		}}
		require.NoError(t, WriteChangeSet(fp, v, cs))
	}
}

func TestVerifyOneStoreBumpsVersionOnGaps(t *testing.T) {
	dir := t.TempDir()

	// "foo" only changed at versions 1 and 3, skipping version 2 entirely - as would happen for
	// a store that had no writes in block 2.
	writeStoreChangeSet(t, dir, "foo", []int64{1, 3})

	tree := memiavl.New(0)
	exists, err := verifyOneStore(tree, "foo", dir, 3)
	require.NoError(t, err)
	require.True(t, exists)

	require.Equal(t, int64(3), tree.Version())
}

func TestVerifyOneStoreCatchesUpToTargetVersion(t *testing.T) {
	dir := t.TempDir()

	writeStoreChangeSet(t, dir, "foo", []int64{1})

	tree := memiavl.New(0)
	exists, err := verifyOneStore(tree, "foo", dir, 5)
	require.NoError(t, err)
	require.True(t, exists)

	require.Equal(t, int64(5), tree.Version())
}

// Without --target-version each store stops at its own last changeset, but a multitree
// snapshot records one commit-info version for all of them and validates the trees
// against it on load - so the laggards must be bumped before the snapshot is written.
func TestVerifySaveSnapshotIsLoadableWithoutTargetVersion(t *testing.T) {
	changeSetDir := t.TempDir()
	snapshotDir := filepath.Join(t.TempDir(), "snapshot")

	writeStoreChangeSet(t, changeSetDir, "foo", []int64{1, 2, 3})
	writeStoreChangeSet(t, changeSetDir, "bar", []int64{1})

	cmd := VerifyChangeSetCmd(nil)
	cmd.SetArgs([]string{
		changeSetDir,
		"--" + flagStores, "foo bar",
		"--" + flagSaveSnapshot, snapshotDir,
		"--" + flagSave,
	})
	require.NoError(t, cmd.Execute())

	mtree, err := memiavl.LoadMultiTree(snapshotDir, false, 0, "")
	require.NoError(t, err)
	defer mtree.Close()

	require.Equal(t, int64(3), mtree.Version())
	for _, name := range []string{"foo", "bar"} {
		tree := mtree.TreeByName(name)
		require.NotNil(t, tree)
		require.Equal(t, int64(3), tree.Version())
	}
}

func TestDedupStores(t *testing.T) {
	require.Equal(t, []string{"foo", "bar"}, dedupStores([]string{"foo", "bar", "foo", "bar", "foo"}))
}

// A store carried in from --load-snapshot has no change sets to replay, but it's
// still part of the store set: dropping it would change the app hash and leave the
// written metadata inconsistent with the trees beside it.
func TestVerifyKeepsLoadedStoresWithoutChangeSets(t *testing.T) {
	changeSetDir := t.TempDir()
	baseDir := filepath.Join(t.TempDir(), "base")
	outDir := filepath.Join(t.TempDir(), "out")

	// A base snapshot holding "foo" and "bar", both at version 1.
	mtree := memiavl.NewEmptyMultiTree(0, 0, "")
	require.NoError(t, mtree.ApplyUpgrades([]*memiavl.TreeNameUpgrade{{Name: "foo"}, {Name: "bar"}}))
	require.NoError(t, mtree.ApplyChangeSet("foo", memiavl.ChangeSet{
		Pairs: []*memiavl.KVPair{{Key: []byte("key"), Value: []byte("value")}},
	}))
	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	pool := pond.New(2, 10)
	defer pool.StopAndWait()
	require.NoError(t, mtree.WriteSnapshot(baseDir, pool))
	require.NoError(t, mtree.Close())

	// Only "foo" has change sets past the snapshot; "bar" has none at all.
	writeStoreChangeSet(t, changeSetDir, "foo", []int64{2, 3})

	cmd := VerifyChangeSetCmd(nil)
	cmd.SetArgs([]string{
		changeSetDir,
		"--" + flagStores, "foo bar",
		"--" + flagLoadSnapshot, baseDir,
		"--" + flagSaveSnapshot, outDir,
		"--" + flagSave,
	})
	require.NoError(t, cmd.Execute())

	loaded, err := memiavl.LoadMultiTree(outDir, false, 0, "")
	require.NoError(t, err)
	defer loaded.Close()

	require.Equal(t, int64(3), loaded.Version())
	for _, name := range []string{"foo", "bar"} {
		tree := loaded.TreeByName(name)
		require.NotNil(t, tree, "%s must survive into the written snapshot", name)
		require.Equal(t, int64(3), tree.Version())
	}
}
