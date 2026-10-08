package client

import (
	"context"
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

const (
	fooStore = "foo"
	barStore = "bar"
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

func TestVerifyOneStore(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	testCases := []struct {
		name          string
		ctx           context.Context
		versions      []int64
		targetVersion int64
		expErr        error
		expVersion    int64
	}{
		{
			// "foo" skips version 2 entirely, as a store with no writes in block 2 would.
			name:          "bumps version on gaps",
			ctx:           context.Background(),
			versions:      []int64{1, 3},
			targetVersion: 3,
			expVersion:    3,
		},
		{
			name:          "catches up to target version",
			ctx:           context.Background(),
			versions:      []int64{1},
			targetVersion: 5,
			expVersion:    5,
		},
		{
			name:       "stops replaying once canceled",
			ctx:        canceled,
			versions:   []int64{1, 2},
			expErr:     context.Canceled,
			expVersion: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeStoreChangeSet(t, dir, fooStore, tc.versions)

			tree := memiavl.New(0)
			exists, err := verifyOneStore(tc.ctx, tree, fooStore, dir, tc.targetVersion)
			if tc.expErr != nil {
				require.ErrorIs(t, err, tc.expErr)
			} else {
				require.NoError(t, err)
				require.True(t, exists)
			}
			require.Equal(t, tc.expVersion, tree.Version())
		})
	}
}

// Without --target-version each store stops at its own last changeset, but a multitree
// snapshot records one commit-info version for all of them and validates the trees
// against it on load - so the laggards must be bumped before the snapshot is written.
func TestVerifySaveSnapshotIsLoadableWithoutTargetVersion(t *testing.T) {
	changeSetDir := t.TempDir()
	snapshotDir := filepath.Join(t.TempDir(), "snapshot")

	writeStoreChangeSet(t, changeSetDir, fooStore, []int64{1, 2, 3})
	writeStoreChangeSet(t, changeSetDir, barStore, []int64{1})

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
	for _, name := range []string{fooStore, barStore} {
		tree := mtree.TreeByName(name)
		require.NotNil(t, tree)
		require.Equal(t, int64(3), tree.Version())
	}
}

func TestVerifyStopsAfterFirstFailedStore(t *testing.T) {
	changeSetDir := t.TempDir()

	// "a" fails on its garbage change-set file. "b" is a file where its directory
	// belongs, so it fails too, but only if its task runs.
	require.NoError(t, os.MkdirAll(filepath.Join(changeSetDir, "a"), os.ModePerm))
	require.NoError(t, os.WriteFile(filepath.Join(changeSetDir, "a", "block-1"), []byte("garbage"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(changeSetDir, "b"), nil, 0o600))

	cmd := VerifyChangeSetCmd(nil)
	cmd.SetArgs([]string{
		changeSetDir,
		"--" + flagStores, "a b",
		// one worker runs the stores in order, so "b" is still queued when "a" fails.
		"--" + flagConcurrency, "1",
	})
	err := cmd.Execute()
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "not a directory", `"b" must be skipped once "a" failed`)
}

func TestNormalizeStores(t *testing.T) {
	testCases := []struct {
		name      string
		stores    []string
		expStores []string
		expErr    string
	}{
		{
			name:      "drops repeats",
			stores:    []string{fooStore, barStore, fooStore, barStore, fooStore},
			expStores: []string{fooStore, barStore},
		},
		{
			name:      "folds path aliases into one store",
			stores:    []string{fooStore, "foo/", "./foo", "foo//", barStore},
			expStores: []string{fooStore, barStore},
		},
		{
			// what a double space in --stores splits into
			name:   "rejects an empty name",
			stores: []string{fooStore, "", barStore},
			expErr: `invalid store name ""`,
		},
		{
			name:   "rejects the current directory",
			stores: []string{"./"},
			expErr: `invalid store name "./"`,
		},
		{
			name:   "rejects the parent directory",
			stores: []string{".."},
			expErr: `invalid store name ".."`,
		},
		{
			name:   "rejects nested paths",
			stores: []string{"../foo"},
			expErr: `invalid store name "../foo"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stores, err := normalizeStores(tc.stores)
			if tc.expErr != "" {
				require.ErrorContains(t, err, tc.expErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expStores, stores)
		})
	}
}

// writeBaseSnapshot writes a snapshot holding "foo" and "bar" at their first version
// (initialVersion, or 1 if that's 0), with one key in "foo", and returns its directory.
func writeBaseSnapshot(t *testing.T, initialVersion uint32) string {
	t.Helper()
	baseDir := filepath.Join(t.TempDir(), "base")

	mtree := memiavl.NewEmptyMultiTree(initialVersion, 0, "")
	require.NoError(t, mtree.ApplyUpgrades([]*memiavl.TreeNameUpgrade{{Name: fooStore}, {Name: barStore}}))
	require.NoError(t, mtree.ApplyChangeSet(fooStore, memiavl.ChangeSet{
		Pairs: []*memiavl.KVPair{{Key: []byte("key"), Value: []byte("value")}},
	}))
	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	pool := pond.New(2, 10)
	defer pool.StopAndWait()
	require.NoError(t, mtree.WriteSnapshot(baseDir, pool))
	require.NoError(t, mtree.Close())
	return baseDir
}

func TestVerifyKeepsLoadedStoresWithoutChangeSets(t *testing.T) {
	testCases := []struct {
		name           string
		initialVersion uint32
		baseVersion    int64
	}{
		{name: "no initial version", initialVersion: 0, baseVersion: 1},
		{name: "initial version above one", initialVersion: 100, baseVersion: 100},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			changeSetDir := t.TempDir()
			baseDir := writeBaseSnapshot(t, tc.initialVersion)
			outDir := filepath.Join(t.TempDir(), "out")
			finalVersion := tc.baseVersion + 2

			// Only "foo" has change sets past the snapshot; "bar" has none at all.
			writeStoreChangeSet(t, changeSetDir, fooStore, []int64{tc.baseVersion + 1, finalVersion})

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

			require.Equal(t, finalVersion, loaded.Version())
			for _, name := range []string{fooStore, barStore} {
				tree := loaded.TreeByName(name)
				require.NotNil(t, tree, "%s must survive into the written snapshot", name)
				require.Equal(t, finalVersion, tree.Version())
			}

			bz, err := os.ReadFile(filepath.Join(outDir, memiavl.MetadataFileName))
			require.NoError(t, err)
			var metadata memiavl.MultiTreeMetadata
			require.NoError(t, metadata.Unmarshal(bz))
			require.EqualValues(t, tc.initialVersion, metadata.InitialVersion)
		})
	}
}

// With --load-snapshot a repeated store name resolves to the same loaded tree, so
// two workers would replay into it concurrently (run under -race). An alias such as
// "foo/" would add a second store over the same change-set and snapshot directories.
func TestVerifyDedupsStoresWithLoadedSnapshot(t *testing.T) {
	changeSetDir := t.TempDir()
	baseDir := writeBaseSnapshot(t, 0)
	outDir := filepath.Join(t.TempDir(), "out")

	writeStoreChangeSet(t, changeSetDir, fooStore, []int64{2, 3})

	cmd := VerifyChangeSetCmd(nil)
	cmd.SetArgs([]string{
		changeSetDir,
		"--" + flagStores, "foo foo/ bar ./foo foo",
		"--" + flagLoadSnapshot, baseDir,
		"--" + flagSaveSnapshot, outDir,
		"--" + flagSave,
	})
	require.NoError(t, cmd.Execute())

	loaded, err := memiavl.LoadMultiTree(outDir, false, 0, "")
	require.NoError(t, err)
	defer loaded.Close()

	require.Len(t, loaded.Trees(), 2, "each store must appear once in the written snapshot")
	require.Equal(t, int64(3), loaded.Version())
	require.Equal(t, []byte("value"), loaded.TreeByName(fooStore).Get([]byte("key")))
}
