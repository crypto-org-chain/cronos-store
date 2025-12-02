package memiavl

import (
	"encoding/hex"
	"errors"
	fmt "fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const TestAppChainID = "test_chain"

func TestRewriteSnapshot(t *testing.T) {
	db, err := Load(t.TempDir(), Options{
		CreateIfMissing: true,
		InitialStores:   []string{"test"},
	}, TestAppChainID)
	require.NoError(t, err)

	for i, changes := range ChangeSets {
		cs := []*NamedChangeSet{
			{
				Name:      "test",
				Changeset: changes,
			},
		}
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			require.NoError(t, db.ApplyChangeSets(cs))
			v, err := db.Commit()
			require.NoError(t, err)
			require.Equal(t, i+1, int(v))
			require.Equal(t, RefHashes[i], db.lastCommitInfo.StoreInfos[0].CommitId.Hash)
			require.NoError(t, db.RewriteSnapshot())
			require.NoError(t, db.Reload())
		})
	}
}

func TestRemoveSnapshotDir(t *testing.T) {
	dbDir := t.TempDir()
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			require.NoError(t, err)
		}
	}(dbDir)

	snapshotDir := filepath.Join(dbDir, snapshotName(0))
	tmpDir := snapshotDir + TmpSuffix
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		t.Fatalf("Failed to create dummy snapshot directory: %v", err)
	}
	db, err := Load(dbDir, Options{
		CreateIfMissing:    true,
		InitialStores:      []string{"test"},
		SnapshotKeepRecent: 0,
	}, TestAppChainID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = os.Stat(tmpDir)
	require.True(t, os.IsNotExist(err), "Expected temporary snapshot directory to be deleted, but it still exists")

	err = os.MkdirAll(tmpDir, os.ModePerm)
	require.NoError(t, err)

	_, err = Load(dbDir, Options{
		ReadOnly: true,
	}, TestAppChainID)
	require.NoError(t, err)

	_, err = os.Stat(tmpDir)
	require.False(t, os.IsNotExist(err))

	db, err = Load(dbDir, Options{}, TestAppChainID)
	require.NoError(t, err)

	_, err = os.Stat(tmpDir)
	require.True(t, os.IsNotExist(err))

	require.NoError(t, db.Close())
}

func TestRewriteSnapshotBackground(t *testing.T) {
	db, err := Load(t.TempDir(), Options{
		CreateIfMissing:    true,
		InitialStores:      []string{"test"},
		SnapshotKeepRecent: 0, // only a single snapshot is kept
	}, TestAppChainID)
	require.NoError(t, err)

	for i, changes := range ChangeSets {
		cs := []*NamedChangeSet{
			{
				Name:      "test",
				Changeset: changes,
			},
		}
		require.NoError(t, db.ApplyChangeSets(cs))
		v, err := db.Commit()
		require.NoError(t, err)
		require.Equal(t, i+1, int(v))
		require.Equal(t, RefHashes[i], db.lastCommitInfo.StoreInfos[0].CommitId.Hash)

		_ = db.RewriteSnapshotBackground()
		time.Sleep(time.Millisecond * 20)
	}

	for db.snapshotRewriteChan != nil {
		require.NoError(t, db.checkAsyncTasks())
	}

	db.pruneSnapshotLock.Lock()
	defer db.pruneSnapshotLock.Unlock()

	entries, err := os.ReadDir(db.dir)
	require.NoError(t, err)

	// three files: snapshot, current link, wal, LOCK
	require.Equal(t, 4, len(entries))
}

func TestWAL(t *testing.T) {
	dir := t.TempDir()
	db, err := Load(dir, Options{CreateIfMissing: true, InitialStores: []string{"test", "delete"}}, TestAppChainID)
	require.NoError(t, err)

	for _, changes := range ChangeSets {
		cs := []*NamedChangeSet{
			{
				Name:      "test",
				Changeset: changes,
			},
		}
		require.NoError(t, db.ApplyChangeSets(cs))
		_, err := db.Commit()
		require.NoError(t, err)
	}

	require.Equal(t, 2, len(db.lastCommitInfo.StoreInfos))

	require.NoError(t, db.ApplyUpgrades([]*TreeNameUpgrade{
		{
			Name:       "newtest",
			RenameFrom: "test",
		},
		{
			Name:   "delete",
			Delete: true,
		},
	}))
	_, err = db.Commit()
	require.NoError(t, err)

	require.NoError(t, db.Close())

	db, err = Load(dir, Options{}, TestAppChainID)
	require.NoError(t, err)

	require.Equal(t, "newtest", db.lastCommitInfo.StoreInfos[0].Name)
	require.Equal(t, 1, len(db.lastCommitInfo.StoreInfos))
	require.Equal(t, RefHashes[len(RefHashes)-1], db.lastCommitInfo.StoreInfos[0].CommitId.Hash)
}

func mockNameChangeSet(name, key, value string) []*NamedChangeSet {
	return []*NamedChangeSet{
		{
			Name: name,
			Changeset: ChangeSet{
				Pairs: mockKVPairs(key, value),
			},
		},
	}
}

// 0/1 -> v :1
// ...
// 100 -> v: 100
func TestInitialVersion(t *testing.T) {
	name := "test"
	name1 := "new"
	name2 := "new2"
	key := "hello"
	value := "world"
	value1 := "world1"
	for _, initialVersion := range []int64{0, 1, 100} {
		dir := t.TempDir()
		db, err := Load(dir, Options{CreateIfMissing: true, InitialStores: []string{name}}, TestAppChainID)
		require.NoError(t, err)
		err = db.SetInitialVersion(initialVersion)
		require.NoError(t, err)
		require.NoError(t, db.ApplyChangeSets(mockNameChangeSet(name, key, value)))
		v, err := db.Commit()
		require.NoError(t, err)

		realInitialVersion := max(initialVersion, 1)
		require.Equal(t, realInitialVersion, v)

		// the nodes are created with initial version to be compatible with iavl v1 behavior.
		// with iavl v0, the nodes are created with version 1.
		commitId := db.LastCommitInfo().StoreInfos[0].CommitId
		require.Equal(t, commitId.Hash, HashNode(newLeafNode([]byte(key), []byte(value), uint32(commitId.Version))))

		require.NoError(t, db.ApplyChangeSets(mockNameChangeSet(name, key, value1)))
		v, err = db.Commit()
		require.NoError(t, err)
		commitId = db.LastCommitInfo().StoreInfos[0].CommitId
		require.Equal(t, realInitialVersion+1, v)
		require.Equal(t, commitId.Hash, HashNode(newLeafNode([]byte(key), []byte(value1), uint32(commitId.Version))))
		require.NoError(t, db.Close())

		// reload the db, check the contents are the same
		db, err = Load(dir, Options{}, TestAppChainID)
		require.NoError(t, err)
		require.Equal(t, uint32(initialVersion), db.initialVersion)
		require.Equal(t, v, db.Version())
		require.Equal(t, hex.EncodeToString(commitId.Hash), hex.EncodeToString(db.LastCommitInfo().StoreInfos[0].CommitId.Hash))

		// add a new store to a reloaded db
		err = db.ApplyUpgrades([]*TreeNameUpgrade{{Name: name1}})
		require.NoError(t, err)
		require.NoError(t, db.ApplyChangeSets(mockNameChangeSet(name1, key, value)))
		v, err = db.Commit()
		require.NoError(t, err)
		require.Equal(t, realInitialVersion+2, v)
		require.Equal(t, 2, len(db.lastCommitInfo.StoreInfos))
		info := db.lastCommitInfo.StoreInfos[0]
		require.Equal(t, name1, info.Name)
		require.Equal(t, v, info.CommitId.Version)
		require.Equal(t, info.CommitId.Hash, HashNode(newLeafNode([]byte(key), []byte(value), uint32(info.CommitId.Version))))

		// test snapshot rewriting and reload
		require.NoError(t, db.RewriteSnapshot())
		require.NoError(t, db.Reload())
		// add new store after snapshot rewriting
		err = db.ApplyUpgrades([]*TreeNameUpgrade{{Name: name2}})
		require.NoError(t, err)
		require.NoError(t, db.ApplyChangeSets(mockNameChangeSet(name2, key, value)))
		v, err = db.Commit()
		require.NoError(t, err)
		require.Equal(t, realInitialVersion+3, v)
		require.Equal(t, 3, len(db.lastCommitInfo.StoreInfos))
		info2 := db.lastCommitInfo.StoreInfos[1]
		require.Equal(t, name2, info2.Name)
		require.Equal(t, v, info2.CommitId.Version)
		require.Equal(t, info2.CommitId.Hash, HashNode(newLeafNode([]byte(key), []byte(value), uint32(info2.CommitId.Version))))
	}
}

func TestLoadVersion(t *testing.T) {
	dir := t.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"test"},
	}, TestAppChainID)
	require.NoError(t, err)

	for i, changes := range ChangeSets {
		cs := []*NamedChangeSet{
			{
				Name:      "test",
				Changeset: changes,
			},
		}
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			require.NoError(t, db.ApplyChangeSets(cs))

			// check the root hash
			require.Equal(t, RefHashes[db.Version()], db.WorkingCommitInfo().StoreInfos[0].CommitId.Hash)

			_, err := db.Commit()
			require.NoError(t, err)
		})
	}
	require.NoError(t, db.Close())

	for v, expItems := range ExpectItems {
		if v == 0 {
			continue
		}
		tmp, err := Load(dir, Options{
			TargetVersion: uint32(v),
			ReadOnly:      true,
		}, TestAppChainID)
		require.NoError(t, err)
		require.Equal(t, RefHashes[v-1], tmp.TreeByName("test").RootHash())
		require.Equal(t, expItems, collectIter(tmp.TreeByName("test").Iterator(nil, nil, true)))
	}
}

func TestTreeTraverseStateChanges(t *testing.T) {
	dir := t.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"test", "other"},
	}, TestAppChainID)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	applyAndCommit := func(changeSets []*NamedChangeSet) {
		require.NoError(t, db.ApplyChangeSets(changeSets))
		_, err := db.Commit()
		require.NoError(t, err)
	}

	applyAndCommit([]*NamedChangeSet{
		{Name: "test", Changeset: ChangeSet{Pairs: mockKVPairs("foo", "bar")}},
	})
	applyAndCommit([]*NamedChangeSet{
		{Name: "other", Changeset: ChangeSet{Pairs: mockKVPairs("baz", "qux")}},
	})
	applyAndCommit([]*NamedChangeSet{
		{Name: "test", Changeset: ChangeSet{Pairs: mockKVPairs("foo", "baz")}},
	})

	firstVersion, err := db.FirstVersion()
	require.NoError(t, err)
	require.EqualValues(t, 1, firstVersion)
	starts, err := db.FirstStoreVersions([]string{"test", "other"})
	require.NoError(t, err)
	require.EqualValues(t, 1, starts["test"])
	require.EqualValues(t, 2, starts["other"])

	tree := db.TreeByName("test")
	require.NotNil(t, tree)

	var versions []int64
	var changeSets []ChangeSet
	require.NoError(t, tree.TraverseStateChanges(0, 10, func(version int64, cs *ChangeSet) error {
		versions = append(versions, version)
		copied := ChangeSet{}
		for _, pair := range cs.Pairs {
			cp := &KVPair{Delete: pair.Delete}
			if len(pair.Key) > 0 {
				cp.Key = append([]byte(nil), pair.Key...)
			}
			if len(pair.Value) > 0 {
				cp.Value = append([]byte(nil), pair.Value...)
			}
			copied.Pairs = append(copied.Pairs, cp)
		}
		changeSets = append(changeSets, copied)
		return nil
	}))

	require.Equal(t, []int64{1, 2, 3}, versions)
	require.Len(t, changeSets, 3)
	require.Len(t, changeSets[0].Pairs, 1)
	require.Equal(t, []byte("foo"), changeSets[0].Pairs[0].Key)
	require.Equal(t, []byte("bar"), changeSets[0].Pairs[0].Value)
	require.Len(t, changeSets[1].Pairs, 0)
	require.Len(t, changeSets[2].Pairs, 1)
	require.Equal(t, []byte("foo"), changeSets[2].Pairs[0].Key)
	require.Equal(t, []byte("baz"), changeSets[2].Pairs[0].Value)
}

func TestTraverseStateChangesWithoutWAL(t *testing.T) {
	dir := t.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"test"},
	}, TestAppChainID)
	require.NoError(t, err)

	require.NoError(t, db.ApplyChangeSets([]*NamedChangeSet{
		{Name: "test", Changeset: ChangeSet{Pairs: mockKVPairs("foo", "bar")}},
	}))
	_, err = db.Commit()
	require.NoError(t, err)
	require.NoError(t, db.RewriteSnapshot())
	require.NoError(t, db.Close())

	walDir := filepath.Join(dir, "wal")
	require.NoError(t, os.RemoveAll(walDir))
	require.NoError(t, os.MkdirAll(walDir, os.ModePerm))

	readonly, err := Load(dir, Options{ReadOnly: true}, TestAppChainID)
	require.NoError(t, err)
	defer func() { require.NoError(t, readonly.Close()) }()

	tree := readonly.TreeByName("test")
	require.NotNil(t, tree)

	err = tree.TraverseStateChanges(1, 1, func(version int64, cs *ChangeSet) error {
		return nil
	})
	require.NoError(t, err)
}

func TestZeroCopy(t *testing.T) {
	db, err := Load(t.TempDir(), Options{InitialStores: []string{"test", "test2"}, CreateIfMissing: true, ZeroCopy: true}, TestAppChainID)
	require.NoError(t, err)
	require.NoError(t, db.ApplyChangeSets([]*NamedChangeSet{
		{Name: "test", Changeset: ChangeSets[0]},
	}))
	_, err = db.Commit()
	require.NoError(t, err)
	require.NoError(t, errors.Join(
		db.RewriteSnapshot(),
		db.Reload(),
	))

	// the test tree's root hash will reference the zero-copy value
	require.NoError(t, db.ApplyChangeSets([]*NamedChangeSet{
		{Name: "test2", Changeset: ChangeSets[0]},
	}))
	_, err = db.Commit()
	require.NoError(t, err)

	commitInfo := *db.LastCommitInfo()

	value := db.TreeByName("test").Get([]byte("hello"))
	require.Equal(t, []byte("world"), value)

	db.SetZeroCopy(false)
	valueCloned := db.TreeByName("test").Get([]byte("hello"))
	require.Equal(t, []byte("world"), valueCloned)

	_ = commitInfo.StoreInfos[0].CommitId.Hash[0]

	require.NoError(t, db.Close())

	require.Equal(t, []byte("world"), valueCloned)

	// accessing the zero-copy value after the db is closed triggers segment fault.
	// reset global panic on fault setting after function finished
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	require.Panics(t, func() {
		require.Equal(t, []byte("world"), value)
	})

	// it's ok to access after db closed
	_ = commitInfo.StoreInfos[0].CommitId.Hash[0]
}

func TestWalIndexConversion(t *testing.T) {
	testCases := []struct {
		index          uint64
		version        int64
		initialVersion uint32
	}{
		{1, 1, 0},
		{1, 1, 1},
		{1, 10, 10},
		{2, 11, 10},
	}
	for _, tc := range testCases {
		require.Equal(t, tc.index, walIndex(tc.version, tc.initialVersion))
		require.Equal(t, tc.version, walVersion(tc.index, tc.initialVersion))
	}
}

func TestEmptyValue(t *testing.T) {
	dir := t.TempDir()
	db, err := Load(dir, Options{InitialStores: []string{"test"}, CreateIfMissing: true, ZeroCopy: true}, TestAppChainID)
	require.NoError(t, err)

	require.NoError(t, db.ApplyChangeSets([]*NamedChangeSet{
		{Name: "test", Changeset: ChangeSet{
			Pairs: []*KVPair{
				{Key: []byte("hello1"), Value: []byte("")},
				{Key: []byte("hello2"), Value: []byte("")},
				{Key: []byte("hello3"), Value: []byte("")},
			},
		}},
	}))
	_, err = db.Commit()
	require.NoError(t, err)

	require.NoError(t, db.ApplyChangeSets([]*NamedChangeSet{
		{Name: "test", Changeset: ChangeSet{
			Pairs: []*KVPair{{Key: []byte("hello1"), Delete: true}},
		}},
	}))
	version, err := db.Commit()
	require.NoError(t, err)

	require.NoError(t, db.Close())

	db, err = Load(dir, Options{ZeroCopy: true}, TestAppChainID)
	require.NoError(t, err)
	require.Equal(t, version, db.Version())
}

func TestInvalidOptions(t *testing.T) {
	dir := t.TempDir()

	_, err := Load(dir, Options{ReadOnly: true}, TestAppChainID)
	require.Error(t, err)

	_, err = Load(dir, Options{ReadOnly: true, CreateIfMissing: true}, TestAppChainID)
	require.Error(t, err)

	db, err := Load(dir, Options{CreateIfMissing: true}, TestAppChainID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Load(dir, Options{LoadForOverwriting: true, ReadOnly: true}, TestAppChainID)
	require.Error(t, err)

	_, err = Load(dir, Options{ReadOnly: true}, TestAppChainID)
	require.NoError(t, err)
}

func TestExclusiveLock(t *testing.T) {
	dir := t.TempDir()

	db, err := Load(dir, Options{CreateIfMissing: true}, TestAppChainID)
	require.NoError(t, err)

	_, err = Load(dir, Options{}, TestAppChainID)
	require.Error(t, err)

	_, err = Load(dir, Options{ReadOnly: true}, TestAppChainID)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	_, err = Load(dir, Options{}, TestAppChainID)
	require.NoError(t, err)
}

func TestFastCommit(t *testing.T) {
	dir := t.TempDir()

	db, err := Load(dir, Options{CreateIfMissing: true, InitialStores: []string{"test"}, SnapshotInterval: 3, AsyncCommitBuffer: 10}, TestAppChainID)
	require.NoError(t, err)

	cs := ChangeSet{
		Pairs: []*KVPair{
			{Key: []byte("hello1"), Value: make([]byte, 1024*1024)},
		},
	}

	// the bug reproduce when the wal writing is slower than commit, that happens when wal segment is full and create a new one, the wal writing will slow down a little bit,
	// segment size is 20m, each change set is 1m, so we need a bit more than 20 commits to reproduce.
	for i := 0; i < 30; i++ {
		require.NoError(t, db.ApplyChangeSets([]*NamedChangeSet{{Name: "test", Changeset: cs}}))
		_, err := db.Commit()
		require.NoError(t, err)
	}

	<-db.snapshotRewriteChan
	require.NoError(t, db.Close())
}

func TestRepeatedApplyChangeSet(t *testing.T) {
	db, err := Load(t.TempDir(), Options{CreateIfMissing: true, InitialStores: []string{"test1", "test2"}, SnapshotInterval: 3, AsyncCommitBuffer: 10}, TestAppChainID)
	require.NoError(t, err)

	err = db.ApplyChangeSets([]*NamedChangeSet{
		{Name: "test1", Changeset: ChangeSet{
			Pairs: []*KVPair{
				{Key: []byte("hello1"), Value: []byte("world1")},
			},
		}},
		{Name: "test2", Changeset: ChangeSet{
			Pairs: []*KVPair{
				{Key: []byte("hello2"), Value: []byte("world2")},
			},
		}},
	})
	require.NoError(t, err)

	err = db.ApplyChangeSets([]*NamedChangeSet{{Name: "test1"}})
	require.NoError(t, err)

	err = db.ApplyChangeSet("test1", ChangeSet{
		Pairs: []*KVPair{
			{Key: []byte("hello2"), Value: []byte("world2")},
		},
	})
	require.NoError(t, err)

	_, err = db.Commit()
	require.NoError(t, err)

	err = db.ApplyChangeSet("test1", ChangeSet{
		Pairs: []*KVPair{
			{Key: []byte("hello2"), Value: []byte("world2")},
		},
	})
	require.NoError(t, err)
	err = db.ApplyChangeSet("test2", ChangeSet{
		Pairs: []*KVPair{
			{Key: []byte("hello2"), Value: []byte("world2")},
		},
	})
	require.NoError(t, err)

	err = db.ApplyChangeSet("test1", ChangeSet{
		Pairs: []*KVPair{
			{Key: []byte("hello2"), Value: []byte("world2")},
		},
	})
	require.NoError(t, err)
	err = db.ApplyChangeSet("test2", ChangeSet{
		Pairs: []*KVPair{
			{Key: []byte("hello2"), Value: []byte("world2")},
		},
	})
	require.NoError(t, err)
}

func TestInsertPendingChangeSetKeepsOldEntryAfterCacheMiss(t *testing.T) {
	db := &DB{}
	original := &NamedChangeSet{
		Name: "bank",
		Changeset: ChangeSet{
			Pairs: []*KVPair{{Key: []byte("k1"), Value: []byte("v1")}},
		},
	}
	db.insertPendingChangeSet(original)

	// simulate the cached map being dropped while the pending log is still populated
	db.cachedPendingChangesets = nil

	newer := &NamedChangeSet{
		Name: "bank",
		Changeset: ChangeSet{
			Pairs: []*KVPair{{Key: []byte("k2"), Value: []byte("v2")}},
		},
	}
	db.insertPendingChangeSet(newer)

	require.Equal(t, newer, db.pendingLog.Changesets[0])
	require.Equal(t, original, db.pendingLog.Changesets[1])

	// rebuilding the cache must pick the oldest entry so that subsequent merges
	// keep appending to the original change set instead of creating new copies.
	db.cachedPendingChangesets = nil
	db.rebuildPendingChangesetMap(db.pendingLog.Changesets)

	require.Same(t, original, db.cachedPendingChangesets["bank"])
}

func TestIdempotentWrite(t *testing.T) {
	for _, asyncCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("asyncCommit=%v", asyncCommit), func(t *testing.T) {
			testIdempotentWrite(t, asyncCommit)
		})
	}
}

func testIdempotentWrite(t *testing.T, asyncCommit bool) {
	t.Helper()
	dir := t.TempDir()

	asyncCommitBuffer := -1
	if asyncCommit {
		asyncCommitBuffer = 10
	}

	db, err := Load(dir, Options{
		CreateIfMissing:   true,
		InitialStores:     []string{"test1", "test2"},
		AsyncCommitBuffer: asyncCommitBuffer,
	}, TestAppChainID)
	require.NoError(t, err)

	// generate some data into db
	var changes [][]*NamedChangeSet
	for i := 0; i < 10; i++ {
		cs := []*NamedChangeSet{
			{
				Name:      "test1",
				Changeset: ChangeSet{Pairs: mockKVPairs("hello", fmt.Sprintf("world%d", i))},
			},
			{
				Name:      "test2",
				Changeset: ChangeSet{Pairs: mockKVPairs("hello", fmt.Sprintf("world%d", i))},
			},
		}
		changes = append(changes, cs)
	}

	for _, cs := range changes {
		require.NoError(t, db.ApplyChangeSets(cs))
		_, err := db.Commit()
		require.NoError(t, err)
	}

	commitInfo := *db.LastCommitInfo()
	require.NoError(t, db.Close())

	// reload db from disk at an intermediate version
	db, err = Load(dir, Options{TargetVersion: 5}, TestAppChainID)
	require.NoError(t, err)

	// replay some random writes to reach same version
	for i := 0; i < 5; i++ {
		require.NoError(t, db.ApplyChangeSets(changes[i+5]))
		_, err := db.Commit()
		require.NoError(t, err)
	}

	// it should reach same result
	require.Equal(t, commitInfo, *db.LastCommitInfo())

	require.NoError(t, db.Close())

	// reload db again, it should reach same result
	db, err = Load(dir, Options{}, TestAppChainID)
	require.NoError(t, err)
	require.Equal(t, commitInfo, *db.LastCommitInfo())
}
