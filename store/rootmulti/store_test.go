package rootmulti

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	protoio "github.com/cosmos/gogoproto/io"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/crypto-org-chain/cronos-store/store/memiavlstore"
	"github.com/stretchr/testify/require"

	log "cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/store/v2/rootmulti"
	snapshottypes "github.com/cosmos/cosmos-sdk/store/v2/snapshots/types"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

const (
	TestAppChainID   = "test_chain"
	testStoreName    = "test"
	orphanStoreName  = "orphan"
	oldStoreName     = "old"
	newStoreName     = "new"
	addedStoreName   = "added"
	deletedStoreName = "deleted"
)

func TestLastCommitID(t *testing.T) {
	store := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	require.Equal(t, types.CommitID{}, store.LastCommitID())
}

func TestLoadLatestVersionRejectsUnexpectedMemiAVLTree(t *testing.T) {
	dir := t.TempDir()
	db, err := memiavl.Load(dir, memiavl.Options{
		CreateIfMissing:   true,
		InitialStores:     []string{orphanStoreName, testStoreName},
		AsyncCommitBuffer: -1,
	}, TestAppChainID)
	require.NoError(t, err)
	_, err = db.Commit()
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.MountStoreWithDB(types.NewKVStoreKey(testStoreName), types.StoreTypeIAVL, nil)

	err = store.LoadLatestVersion()
	require.ErrorContains(t, err, "memiavl tree membership mismatch")
	require.ErrorContains(t, err, "unexpected=[orphan]")
}

// setupOldStoreAtVersion2 creates a memiavl db with oldStoreName holding key
// "k"->"v" and testStoreName at version 1, then applies upgrades and commits
// version 2, closing the db afterward.
func setupOldStoreAtVersion2(t *testing.T, dir string, upgrades []*memiavl.TreeNameUpgrade) {
	t.Helper()

	db, err := memiavl.Load(dir, memiavl.Options{
		CreateIfMissing:   true,
		InitialStores:     []string{oldStoreName, testStoreName},
		AsyncCommitBuffer: -1,
	}, TestAppChainID)
	require.NoError(t, err)
	require.NoError(t, db.ApplyChangeSet(oldStoreName, memiavl.ChangeSet{Pairs: []*memiavl.KVPair{{Key: []byte("k"), Value: []byte("v")}}}))
	_, err = db.Commit()
	require.NoError(t, err)
	require.NoError(t, db.ApplyUpgrades(upgrades))
	_, err = db.Commit()
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func TestLoadVersionAllowsHistoricalMemiAVLTreeMembership(t *testing.T) {
	dir := t.TempDir()
	setupOldStoreAtVersion2(t, dir, []*memiavl.TreeNameUpgrade{{Name: oldStoreName, Delete: true}})

	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.MountStoreWithDB(types.NewKVStoreKey(testStoreName), types.StoreTypeIAVL, nil)

	require.NoError(t, store.LoadVersion(1))
	require.NotNil(t, store.db.TreeByName(oldStoreName))
	require.NoError(t, store.Close())
}

func TestLoadVersionAndUpgradeAllowsHistoricalMemiAVLTreeMembershipWithEmptyUpgrades(t *testing.T) {
	dir := t.TempDir()
	setupOldStoreAtVersion2(t, dir, []*memiavl.TreeNameUpgrade{{Name: oldStoreName, Delete: true}})

	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.MountStoreWithDB(types.NewKVStoreKey(testStoreName), types.StoreTypeIAVL, nil)

	// A non-nil but empty StoreUpgrades must not force the exact-membership
	// check used for latest/upgrade loads onto a historical load.
	require.NoError(t, store.LoadVersionAndUpgrade(1, &types.StoreUpgrades{}))
	require.NotNil(t, store.db.TreeByName(oldStoreName))
	require.NoError(t, store.Close())
}

type storeUpgradeCase struct {
	name          string
	initialStores []string
	mountedStores []string
	upgrades      *types.StoreUpgrades
	dataStore     string
	loadedStore   string
}

// storeUpgradeCases adds, deletes or renames one store beside testStoreName.
func storeUpgradeCases() []storeUpgradeCase {
	return []storeUpgradeCase{
		{
			name:          "add",
			initialStores: []string{testStoreName},
			mountedStores: []string{addedStoreName, testStoreName},
			upgrades:      &types.StoreUpgrades{Added: []string{addedStoreName}},
		},
		{
			name:          "delete",
			initialStores: []string{deletedStoreName, testStoreName},
			mountedStores: []string{testStoreName},
			upgrades:      &types.StoreUpgrades{Deleted: []string{deletedStoreName}},
		},
		{
			name:          "rename",
			initialStores: []string{oldStoreName, testStoreName},
			mountedStores: []string{newStoreName, testStoreName},
			upgrades: &types.StoreUpgrades{Renamed: []types.StoreRename{{
				OldKey: oldStoreName,
				NewKey: newStoreName,
			}}},
			dataStore:   oldStoreName,
			loadedStore: newStoreName,
		},
	}
}

func TestLoadLatestVersionAndUpgradeValidatesMemiAVLTreeMembership(t *testing.T) {
	for _, tc := range storeUpgradeCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := memiavl.Load(dir, memiavl.Options{
				CreateIfMissing:   true,
				InitialStores:     tc.initialStores,
				AsyncCommitBuffer: -1,
			}, TestAppChainID)
			require.NoError(t, err)
			if tc.dataStore != "" {
				require.NoError(t, db.ApplyChangeSet(tc.dataStore, memiavl.ChangeSet{Pairs: []*memiavl.KVPair{{Key: []byte("k"), Value: []byte("v")}}}))
			}
			_, err = db.Commit()
			require.NoError(t, err)
			require.NoError(t, db.Close())

			store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
			keys := make(map[string]*types.KVStoreKey, len(tc.mountedStores))
			for _, name := range tc.mountedStores {
				key := types.NewKVStoreKey(name)
				keys[name] = key
				store.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
			}

			require.NoError(t, store.LoadLatestVersionAndUpgrade(tc.upgrades))
			require.Len(t, store.db.Trees(), len(tc.mountedStores))
			for _, name := range tc.mountedStores {
				require.NotNil(t, store.db.TreeByName(name))
			}
			if tc.loadedStore != "" {
				require.Equal(t, []byte("v"), store.GetKVStore(keys[tc.loadedStore]).Get([]byte("k")))
			}
			require.NoError(t, store.Close())
		})
	}
}

func TestLoadLatestVersionAndUpgradeRejectsUnexpectedMemiAVLTree(t *testing.T) {
	tests := []struct {
		name          string
		initialStores []string
		mountedStores []string
		upgrades      *types.StoreUpgrades
	}{
		{
			name:          "add",
			initialStores: []string{orphanStoreName, testStoreName},
			mountedStores: []string{addedStoreName, testStoreName},
			upgrades:      &types.StoreUpgrades{Added: []string{addedStoreName}},
		},
		{
			name:          "delete",
			initialStores: []string{deletedStoreName, orphanStoreName, testStoreName},
			mountedStores: []string{testStoreName},
			upgrades:      &types.StoreUpgrades{Deleted: []string{deletedStoreName}},
		},
		{
			name:          "rename",
			initialStores: []string{oldStoreName, orphanStoreName, testStoreName},
			mountedStores: []string{newStoreName, testStoreName},
			upgrades: &types.StoreUpgrades{Renamed: []types.StoreRename{{
				OldKey: oldStoreName,
				NewKey: newStoreName,
			}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := memiavl.Load(dir, memiavl.Options{
				CreateIfMissing:   true,
				InitialStores:     tc.initialStores,
				AsyncCommitBuffer: -1,
			}, TestAppChainID)
			require.NoError(t, err)
			_, err = db.Commit()
			require.NoError(t, err)
			require.NoError(t, db.Close())

			store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
			for _, name := range tc.mountedStores {
				store.MountStoreWithDB(types.NewKVStoreKey(name), types.StoreTypeIAVL, nil)
			}

			err = store.LoadLatestVersionAndUpgrade(tc.upgrades)
			require.ErrorContains(t, err, "memiavl tree membership mismatch")
			require.ErrorContains(t, err, "unexpected=[orphan]")
		})
	}
}

// newTestStore creates a rootmulti Store with one IAVL sub-store ("test") mounted,
// loaded, and committed numVersions times so that historical queries are possible.
// The store uses SnapshotInterval=1 so every commit creates a snapshot.
func newTestStore(t *testing.T, numVersions int) (*Store, []int64) {
	t.Helper()

	dir := t.TempDir()
	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.SetMemIAVLOptions(memiavl.Options{
		SnapshotInterval:   1,
		SnapshotKeepRecent: uint32(numVersions + 1),
	})

	key := types.NewKVStoreKey("test")
	store.MountStoreWithDB(key, types.StoreTypeIAVL, nil)

	require.NoError(t, store.LoadLatestVersion())

	versions := make([]int64, 0, numVersions)
	for i := 0; i < numVersions; i++ {
		// Write a key so the tree is non-empty.
		kvStore := store.GetKVStore(key)
		kvStore.Set([]byte("k"), []byte{byte(i)})
		cid := store.Commit()
		versions = append(versions, cid.Version)
	}

	// Wait for any background snapshot writes to complete.
	require.NoError(t, store.db.WaitAsyncCommit())

	return store, versions
}

// TestHistoricalDBCacheReuse verifies that repeated queries to the same
// historical version result in only one memiavl.Load call.
func TestHistoricalDBCacheReuse(t *testing.T) {
	numVersions := 3
	store, versions := newTestStore(t, numVersions)
	defer store.Close()

	// Query an earlier version (not the current one).
	targetVersion := versions[0]

	cache := store.historicalDBCache
	require.NotNil(t, cache)

	var loadCount int32
	loadFn := func() (*memiavl.DB, error) {
		atomic.AddInt32(&loadCount, 1)
		opts := store.opts
		opts.TargetVersion = uint32(targetVersion)
		opts.ReadOnly = true
		return memiavl.Load(store.dir, opts, store.chainId)
	}

	// Borrow the entry multiple times.
	const borrowTimes = 5
	entries := make([]*historicalDBEntry, borrowTimes)
	for i := 0; i < borrowTimes; i++ {
		entry, err := cache.borrow(targetVersion, loadFn)
		require.NoError(t, err)
		entries[i] = entry
	}

	// All borrows should return the same entry with refs accumulated.
	for i := 1; i < borrowTimes; i++ {
		require.Equal(t, entries[0], entries[i], "all borrows should return same entry")
	}

	// Load should have been called only once (subsequent borrows hit the cache).
	require.Equal(t, int32(1), atomic.LoadInt32(&loadCount), "memiavl.Load should be called only once")

	// Release all borrows.
	for _, e := range entries {
		cache.release(e)
	}

	// Entry should still be in the cache (not evicted) with refs==0.
	cache.mu.Lock()
	found := false
	for _, e := range cache.entries {
		if e.version == targetVersion {
			found = true
			require.Equal(t, 0, e.refs)
			require.False(t, e.evicted)
		}
	}
	cache.mu.Unlock()
	require.True(t, found, "entry should still be in cache after all releases")
}

// TestHistoricalDBCacheEviction verifies that when more than maxSize distinct
// versions are queried, the oldest entry is evicted from the cache.
func TestHistoricalDBCacheEviction(t *testing.T) {
	maxSize := 2
	numVersions := maxSize + 2 // need more versions than the cache can hold
	store, versions := newTestStore(t, numVersions)
	defer store.Close()

	// Use a small cache so eviction happens quickly.
	smallCache := newHistoricalDBCache(maxSize)

	// Track the entries we borrow.
	entries := make([]*historicalDBEntry, numVersions)
	for i, v := range versions {
		entry, err := smallCache.borrow(v, func() (*memiavl.DB, error) {
			opts := store.opts
			opts.TargetVersion = uint32(v)
			opts.ReadOnly = true
			return memiavl.Load(store.dir, opts, store.chainId)
		})
		require.NoError(t, err)
		entries[i] = entry
		// Release immediately so refs drop to 0.
		smallCache.release(entry)
	}

	// After loading numVersions entries into a cache of maxSize, only the most
	// recently used maxSize entries should remain.
	smallCache.mu.Lock()
	cacheLen := len(smallCache.entries)
	cachedVersions := make(map[int64]bool)
	for _, e := range smallCache.entries {
		cachedVersions[e.version] = true
	}
	smallCache.mu.Unlock()

	require.Equal(t, maxSize, cacheLen, "cache should hold exactly maxSize entries")

	// The oldest entries (versions[0] and versions[1]) should have been evicted.
	for i := 0; i < numVersions-maxSize; i++ {
		require.False(t, cachedVersions[versions[i]], "oldest versions should be evicted")
	}
	// The newest entries should remain.
	for i := numVersions - maxSize; i < numVersions; i++ {
		require.True(t, cachedVersions[versions[i]], "newest versions should be cached")
	}

	// Close the small cache (not the store's).
	require.NoError(t, smallCache.close())
}

// TestHistoricalDBCacheConcurrent verifies that the cache is goroutine-safe
// when multiple goroutines query different historical versions simultaneously.
func TestHistoricalDBCacheConcurrent(t *testing.T) {
	numVersions := 6
	store, versions := newTestStore(t, numVersions)
	defer store.Close()

	cache := newHistoricalDBCache(3) // smaller than numVersions to force evictions

	var wg sync.WaitGroup
	const goroutines = 10

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := versions[i%numVersions]
			entry, err := cache.borrow(v, func() (*memiavl.DB, error) {
				opts := store.opts
				opts.TargetVersion = uint32(v)
				opts.ReadOnly = true
				return memiavl.Load(store.dir, opts, store.chainId)
			})
			if err != nil {
				t.Errorf("borrow version %d: %v", v, err)
				return
			}
			if entry.db == nil {
				t.Errorf("borrow version %d: got nil db", v)
				cache.release(entry)
				return
			}
			cache.release(entry)
		}(i)
	}

	wg.Wait()

	// Clean up the cache created for this test.
	require.NoError(t, cache.close())
}

// newLoadedStore mounts testStoreName and closes the store at cleanup.
func newLoadedStore(t *testing.T, opts memiavl.Options) (*Store, types.StoreKey) {
	t.Helper()

	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	rs.SetMemIAVLOptions(opts)
	key := types.NewKVStoreKey(testStoreName)
	rs.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	require.NoError(t, rs.LoadLatestVersion())
	// Close is a no-op on a store the test already closed.
	t.Cleanup(func() { require.NoError(t, rs.Close()) })
	return rs, key
}

func rewriteAndReload(t *testing.T, rs *Store) {
	t.Helper()
	require.NoError(t, rs.db.RewriteSnapshot())
	require.NoError(t, rs.db.Reload())
}

func TestRollbackToVersionRebuildsStores(t *testing.T) {
	dir := t.TempDir()
	rs := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)

	key := types.NewKVStoreKey("test")
	rs.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	require.NoError(t, rs.LoadLatestVersion())

	rs.GetKVStore(key).Set([]byte("k"), []byte("v1"))
	rs.Commit()
	rs.GetKVStore(key).Set([]byte("k"), []byte("v2"))
	rs.Commit()

	require.NoError(t, rs.RollbackToVersion(1))
	t.Cleanup(func() { rs.Close() })

	require.Equal(t, []byte("v1"), rs.GetKVStore(key).Get([]byte("k")))
}

func TestCacheMultiStoreWithVersionZeroIsReadOnly(t *testing.T) {
	rs, key := newLoadedStore(t, memiavl.Options{})
	rs.AddListeners([]types.StoreKey{key})

	rs.Commit()

	cms, err := rs.CacheMultiStoreWithVersion(0)
	require.NoError(t, err)
	cms.GetKVStore(key).Set([]byte("k"), []byte("v"))
	cms.Write()

	// the dropped write must not reach the listener stream either.
	require.Empty(t, rs.listeners[key].PopStateCache())
	rs.Commit()
	require.Nil(t, rs.GetKVStore(key).Get([]byte("k")))
}

func TestHistoricalDBCacheDiscardsLoadRacingInvalidate(t *testing.T) {
	store, versions := newTestStore(t, 2)
	defer store.Close()

	cache := store.historicalDBCache
	targetVersion := versions[0]
	loadFn := func() (*memiavl.DB, error) {
		return loadAtVersion(store.dir, store.opts, store.chainId, targetVersion)
	}

	var loaded *memiavl.DB
	_, err := cache.borrow(targetVersion, func() (*memiavl.DB, error) {
		db, err := loadFn()
		loaded = db
		// a reload lands between the load and caching its result.
		require.NoError(t, cache.invalidate())
		return db, err
	})
	require.ErrorContains(t, err, "reloaded while loading")
	require.Empty(t, loaded.Trees(), "the discarded DB must be closed")
	cache.mu.Lock()
	require.Empty(t, cache.entries)
	cache.mu.Unlock()

	entry, err := cache.borrow(targetVersion, loadFn)
	require.NoError(t, err)
	defer cache.release(entry)
	cache.mu.Lock()
	require.Len(t, cache.entries, 1)
	cache.mu.Unlock()
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

// TestCacheMultiStoreWithVersionFutureHeight verifies that requesting a
// height beyond the latest committed version errors instead of silently
// serving state from the latest version (memiavl.Load's fallback behavior).
func TestCacheMultiStoreWithVersionFutureHeight(t *testing.T) {
	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)

	key := types.NewKVStoreKey("test")
	rs.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	require.NoError(t, rs.LoadLatestVersion())
	t.Cleanup(func() { rs.Close() })

	commitID := rs.Commit()
	require.Equal(t, int64(1), commitID.Version)

	_, err := rs.CacheMultiStoreWithVersion(100)
	require.Error(t, err)
}

// TestQueryFutureHeight is the Query() analog of
// TestCacheMultiStoreWithVersionFutureHeight, covering the cached load path.
func TestQueryFutureHeight(t *testing.T) {
	store, _ := newTestStore(t, 2)
	t.Cleanup(func() { store.Close() })

	_, err := store.Query(&types.RequestQuery{Path: "/test/key", Data: []byte("k"), Height: 100})
	require.Error(t, err)
}

func TestQueryUnknownStore(t *testing.T) {
	store, _ := newTestStore(t, 2)
	t.Cleanup(func() { store.Close() })

	res, err := store.Query(&types.RequestQuery{Path: "/doesnotexist/key", Data: []byte("k")})
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "doesnotexist")
	require.ErrorIs(t, err, sdkerrors.ErrUnknownRequest)
}

func TestQueryEmptyStoreName(t *testing.T) {
	store, _ := newTestStore(t, 2)
	t.Cleanup(func() { store.Close() })

	for _, path := range []string{"/", "//key"} {
		res, err := store.Query(&types.RequestQuery{Path: path, Data: []byte("k")})
		require.Error(t, err, "path %q", path)
		require.Nil(t, res, "path %q", path)
		require.ErrorIs(t, err, sdkerrors.ErrUnknownRequest, "path %q", path)
	}
}

func TestQueryHistoricalHeightAllowsDeletedStore(t *testing.T) {
	dir := t.TempDir()
	setupOldStoreAtVersion2(t, dir, []*memiavl.TreeNameUpgrade{{Name: oldStoreName, Delete: true}})

	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.MountStoreWithDB(types.NewKVStoreKey(testStoreName), types.StoreTypeIAVL, nil)
	require.NoError(t, store.LoadLatestVersion())
	t.Cleanup(func() { store.Close() })

	res, err := store.Query(&types.RequestQuery{Path: "/old/key", Data: []byte("k"), Height: 1})
	require.NoError(t, err)
	require.Equal(t, []byte("v"), res.Value)
}

func TestQueryHistoricalHeightAllowsRenamedStore(t *testing.T) {
	dir := t.TempDir()
	setupOldStoreAtVersion2(t, dir, []*memiavl.TreeNameUpgrade{{Name: newStoreName, RenameFrom: oldStoreName}})

	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.MountStoreWithDB(types.NewKVStoreKey(newStoreName), types.StoreTypeIAVL, nil)
	store.MountStoreWithDB(types.NewKVStoreKey(testStoreName), types.StoreTypeIAVL, nil)
	require.NoError(t, store.LoadLatestVersion())
	t.Cleanup(func() { store.Close() })

	res, err := store.Query(&types.RequestQuery{Path: "/old/key", Data: []byte("k"), Height: 1})
	require.NoError(t, err)
	require.Equal(t, []byte("v"), res.Value)
}

func TestLatestHeightReadsOutliveASnapshotRewrite(t *testing.T) {
	testCases := []struct {
		name string
		// read returns the value of key and releases whatever it holds of the snapshot.
		read func(t *testing.T, rs *Store, key types.StoreKey) []byte
	}{
		{
			name: "Query",
			read: func(t *testing.T, rs *Store, _ types.StoreKey) []byte {
				t.Helper()
				res, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k")})
				require.NoError(t, err)
				return res.Value
			},
		},
		{
			name: "CacheMultiStoreWithVersion(0)",
			read: func(t *testing.T, rs *Store, key types.StoreKey) []byte {
				t.Helper()
				cms, err := rs.CacheMultiStoreWithVersion(0)
				require.NoError(t, err)
				value := cms.GetKVStore(key).Get([]byte("k"))
				require.NoError(t, cms.(io.Closer).Close())
				return value
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rs, key := newLoadedStore(t, memiavl.Options{ZeroCopy: true})

			want := bytes.Repeat([]byte("v"), 1<<16)
			rs.GetKVStore(key).Set([]byte("k"), want)
			rs.Commit()
			// publish a snapshot whose trees read from the mmap'd snapshot files.
			rewriteAndReload(t, rs)
			rs.Commit()

			value := tc.read(t, rs, key)

			// the DB keeps the retired generation mapped until its next reload.
			rs.GetKVStore(key).Set([]byte("n"), []byte("v"))
			rs.Commit()
			rewriteAndReload(t, rs)
			rs.Commit()

			// a read of an unmapped page panics here instead of crashing the test binary.
			defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
			require.Equal(t, want, value)
		})
	}
}

func TestQuerySnapshotFollowsZeroCopy(t *testing.T) {
	testCases := []struct {
		zeroCopy   bool
		wantAllocs float64
	}{
		{zeroCopy: false, wantAllocs: 1},
		{zeroCopy: true, wantAllocs: 0},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("zeroCopy=%v", tc.zeroCopy), func(t *testing.T) {
			rs, key := newLoadedStore(t, memiavl.Options{ZeroCopy: tc.zeroCopy})
			want := bytes.Repeat([]byte("v"), 1<<10)
			rs.GetKVStore(key).Set([]byte("k"), want)
			rs.Commit()
			// a tree created empty is zero-copy until it is reloaded from a snapshot.
			rewriteAndReload(t, rs)
			rs.Commit()

			snap := rs.acquireQuerySnapshot()
			defer rs.releaseQuerySnapshot(snap)
			tree := snap.db.TreeByName(testStoreName)
			k := []byte("k")
			var got []byte
			require.Equal(t, tc.wantAllocs, testing.AllocsPerRun(100, func() { got = tree.Get(k) }))
			require.Equal(t, want, got)
		})
	}
}

func TestLoadAtVersionDisablesZeroCopy(t *testing.T) {
	dir := t.TempDir()
	db, err := memiavl.Load(dir, memiavl.Options{
		CreateIfMissing:   true,
		InitialStores:     []string{testStoreName},
		AsyncCommitBuffer: -1,
	}, TestAppChainID)
	require.NoError(t, err)
	require.NoError(t, db.ApplyChangeSet(testStoreName, memiavl.ChangeSet{Pairs: []*memiavl.KVPair{{Key: []byte("k"), Value: bytes.Repeat([]byte("v"), 64)}}}))
	_, err = db.Commit()
	require.NoError(t, err)
	require.NoError(t, db.WaitAsyncCommit())
	// force a real on-disk snapshot so loadAtVersion loads an mmap-backed PersistedNode tree, not a WAL-replayed one
	require.NoError(t, db.RewriteSnapshot())
	require.NoError(t, db.Close())

	// CacheSize: 0 forces every Get through the real path instead of a cache hit
	opts := memiavl.Options{ZeroCopy: true, CacheSize: 0}
	historical, err := loadAtVersion(dir, opts, TestAppChainID, 1)
	require.NoError(t, err)
	defer historical.Close()

	tree := historical.TreeByName(testStoreName)
	require.NotNil(t, tree)

	want := bytes.Repeat([]byte("v"), 64)
	first := tree.Get([]byte("k"))
	second := tree.Get([]byte("k"))
	require.Equal(t, want, first)
	// compare backing addresses, not require.NotEqual (which follows pointers and would treat equal content as equal)
	firstAddr := uintptr(unsafe.Pointer(unsafe.SliceData(first)))
	secondAddr := uintptr(unsafe.Pointer(unsafe.SliceData(second)))
	require.NotEqual(t, firstAddr, secondAddr,
		"loadAtVersion must force ZeroCopy off so historical reads are cloned, not aliased to the mmap")
}

func TestHistoricalQueryResultSurvivesCacheEviction(t *testing.T) {
	numVersions := defaultHistoricalDBCacheSize + 1
	dir := t.TempDir()
	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	store.SetMemIAVLOptions(memiavl.Options{
		// larger than numVersions so the background rewrite never fires; snapshots are written synchronously below instead
		SnapshotInterval:   uint32(numVersions + 10),
		SnapshotKeepRecent: uint32(numVersions + 1),
		ZeroCopy:           true, // simulates operator's memiavl.zero-copy=true
		CacheSize:          0,
	})

	key := types.NewKVStoreKey(testStoreName)
	store.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	require.NoError(t, store.LoadLatestVersion())
	t.Cleanup(func() { store.Close() })

	values := make([][]byte, numVersions)
	versions := make([]int64, numVersions)
	for i := 0; i < numVersions; i++ {
		values[i] = []byte(fmt.Sprintf("value-%d", i))
		kvStore := store.GetKVStore(key)
		kvStore.Set([]byte("k"), values[i])
		cid := store.Commit()
		versions[i] = cid.Version
		require.NoError(t, store.db.WaitAsyncCommit())
		require.NoError(t, store.db.RewriteSnapshot())
	}
	// one more commit so no queried version is current, else Query would read the latest-height snapshot instead of historicalDBCache
	finalCid := store.Commit()
	require.Greater(t, finalCid.Version, versions[numVersions-1])
	require.NoError(t, store.db.WaitAsyncCommit())

	path := "/" + testStoreName + "/key"

	// this entry gets evicted once defaultHistoricalDBCacheSize more distinct versions are queried after it
	res0, err := store.Query(&types.RequestQuery{Path: path, Data: []byte("k"), Height: versions[0], Prove: true})
	require.NoError(t, err)
	require.Equal(t, values[0], res0.Value)
	require.NotNil(t, res0.ProofOps)
	require.NotEmpty(t, res0.ProofOps.Ops)

	// the last of these evicts version[0]'s entry with refs==0, closing it synchronously
	for i := 1; i <= defaultHistoricalDBCacheSize; i++ {
		_, err := store.Query(&types.RequestQuery{Path: path, Data: []byte("k"), Height: versions[i], Prove: true})
		require.NoError(t, err)
	}

	store.historicalDBCache.mu.Lock()
	evicted := true
	for _, e := range store.historicalDBCache.entries {
		if e.version == versions[0] {
			evicted = false
		}
	}
	store.historicalDBCache.mu.Unlock()
	require.True(t, evicted, "version[0]'s entry should have been evicted")

	// a result still aliasing the unmapped snapshot faults; make that a failing panic instead of killing the test binary
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	require.Equal(t, values[0], res0.Value)
	require.NotEmpty(t, res0.ProofOps.Ops)
}

func TestCacheMultiStoreWithVersionHistoricalHeightSkipsDeletedStore(t *testing.T) {
	dir := t.TempDir()
	setupOldStoreAtVersion2(t, dir, []*memiavl.TreeNameUpgrade{{Name: oldStoreName, Delete: true}})

	store := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
	testKey := types.NewKVStoreKey(testStoreName)
	store.MountStoreWithDB(testKey, types.StoreTypeIAVL, nil)
	require.NoError(t, store.LoadLatestVersion())
	t.Cleanup(func() { store.Close() })

	cms, err := store.CacheMultiStoreWithVersion(1)
	require.NoError(t, err)
	require.NotPanics(t, func() { cms.GetKVStore(testKey) })
}

func TestTornDownStoreServesNoReads(t *testing.T) {
	// a stream the importer rejects, so Restore fails before reloading.
	var rejected bytes.Buffer
	w := protoio.NewDelimitedWriter(&rejected)
	require.NoError(t, w.WriteMsg(&snapshottypes.SnapshotItem{
		Item: &snapshottypes.SnapshotItem_IAVL{
			IAVL: &snapshottypes.SnapshotIAVLItem{Key: []byte("k"), Value: []byte("v"), Height: 0, Version: 1},
		},
	}))
	require.NoError(t, w.Close())

	testCases := []struct {
		name     string
		teardown func(rs *Store) error
		// a reload that fails after closing the db also drops the mounted stores and cached historical dbs.
		failedReload bool
	}{
		{"Close", (*Store).Close, false},
		// Restore's teardown.
		{"closeDB", (*Store).closeDB, false},
		{"failed LoadLatestVersion", func(rs *Store) error {
			// a mounted store with no tree and no upgrade adding it fails the reload after the old db closed.
			rs.MountStoreWithDB(types.NewKVStoreKey("unknown"), types.StoreTypeIAVL, nil)
			return rs.LoadLatestVersion()
		}, true},
		{"failed Restore", func(rs *Store) error {
			r := protoio.NewDelimitedReader(bytes.NewReader(rejected.Bytes()), 1<<20)
			defer r.Close()
			_, err := rs.Restore(1, 1, r)
			return err
		}, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rs, key := newLoadedStore(t, memiavl.Options{})
			for _, v := range []string{"v1", "v2"} {
				rs.GetKVStore(key).Set([]byte("k"), []byte(v))
				rs.Commit()
			}
			// populate the historical cache.
			_, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Height: 1})
			require.NoError(t, err)
			require.Len(t, rs.historicalDBCache.entries, 1)

			err = tc.teardown(rs)
			if tc.failedReload {
				require.Error(t, err)
				require.Nil(t, rs.GetCommitStore(key), "stores over the closed trees must be dropped")
				require.Empty(t, rs.historicalDBCache.entries, "cached historical dbs must be dropped with the directory")
			} else {
				require.NoError(t, err)
			}

			require.Nil(t, rs.db)
			require.Nil(t, rs.querySnapshot.Load(), "the querySnapshot must be dropped before rs.db closes")
			require.NotPanics(t, func() {
				require.Zero(t, rs.LatestVersion())
				require.Zero(t, rs.EarliestVersion())

				_, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k")})
				require.ErrorContains(t, err, "store is not loaded")

				_, err = rs.CacheMultiStoreWithVersion(0)
				require.ErrorContains(t, err, "store is not loaded")
			})
			require.Empty(t, rs.LastCommitID().Hash, "no db is loaded, so no hash may be reported")
			// the version still comes from disk: 0 would make CometBFT replay from genesis.
			require.Equal(t, int64(2), rs.LastCommitID().Version)
		})
	}
}

func TestCommitHashesAgreeAcrossReadPaths(t *testing.T) {
	lastHash := make(map[bool][]byte)
	for _, sdk46Compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("sdk46Compact=%v", sdk46Compact), func(t *testing.T) {
			rs := NewStore(t.TempDir(), log.NewNopLogger(), sdk46Compact, false, TestAppChainID)
			key := types.NewKVStoreKey(testStoreName)
			rs.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
			// sdk 0.46 compat hashes mem stores into the app hash.
			rs.MountStoreWithDB(types.NewMemoryStoreKey("mem"), types.StoreTypeMemory, nil)
			require.NoError(t, rs.LoadLatestVersion())
			t.Cleanup(func() { rs.Close() })
			// the genesis app hash has no stores in it, mem stores included.
			require.Equal(t, (&types.CommitInfo{}).CommitID(), rs.LastCommitID())

			verify := func(cid types.CommitID, value []byte) {
				res, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Height: cid.Version, Prove: true})
				require.NoError(t, err)
				require.NoError(t, rootmulti.DefaultProofRuntime().VerifyValue(res.ProofOps, cid.Hash, "/"+testStoreName+"/k", value))
			}
			var cids []types.CommitID
			for i := byte(1); i <= 3; i++ {
				value := []byte{i}
				rs.GetKVStore(key).Set([]byte("k"), value)
				workingHash := rs.WorkingHash()
				cid := rs.Commit()
				require.Equal(t, workingHash, cid.Hash)
				require.Equal(t, cid, rs.LastCommitID())
				verify(cid, value)
				cids = append(cids, cid)
				lastHash[sdk46Compact] = cid.Hash
			}
			// older heights prove from a db loaded from disk, not the published snapshot.
			for i, cid := range cids[:len(cids)-1] {
				verify(cid, []byte{byte(i + 1)})
			}
		})
	}
	require.NotEqual(t, lastHash[false], lastHash[true])
}

func TestLoadLatestVersionReloadsALoadedStore(t *testing.T) {
	rs, key := newLoadedStore(t, memiavl.Options{})
	rs.GetKVStore(key).Set([]byte("k"), []byte("v1"))
	rs.Commit()
	rs.GetKVStore(key).Set([]byte("k"), []byte("v2"))
	cid := rs.Commit()

	_, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Height: 1})
	require.NoError(t, err)
	require.Len(t, rs.historicalDBCache.entries, 1)
	held, err := rs.CacheMultiStoreWithVersion(0)
	require.NoError(t, err)

	require.NoError(t, rs.LoadLatestVersion())
	require.Empty(t, rs.historicalDBCache.entries)
	require.Equal(t, cid, rs.LastCommitID())
	res, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k")})
	require.NoError(t, err)
	require.Equal(t, []byte("v2"), res.Value)

	// the reload closed the db the held reader came from; its snapshot stays mapped.
	require.Equal(t, []byte("v2"), held.GetKVStore(key).Get([]byte("k")))
	require.NoError(t, held.(io.Closer).Close())
}

func TestRestorePublishesTheRestoredHeight(t *testing.T) {
	key := types.NewKVStoreKey(testStoreName)
	src := NewStore(t.TempDir(), log.NewNopLogger(), false, true, TestAppChainID)
	src.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	require.NoError(t, src.LoadLatestVersion())
	t.Cleanup(func() { src.Close() })
	src.GetKVStore(key).Set([]byte("k"), []byte("v1"))
	src.Commit()
	src.GetKVStore(key).Set([]byte("k"), []byte("v2"))
	cid := src.Commit()
	// the exporter reloads from disk, and Commit returns before the async WAL write lands.
	require.NoError(t, src.db.WaitAsyncCommit())

	var buf bytes.Buffer
	w := protoio.NewDelimitedWriter(&buf)
	require.NoError(t, src.Snapshot(uint64(cid.Version), w))
	require.NoError(t, w.Close())

	// state sync restores into a node that loaded an empty store.
	dst, dstKey := newLoadedStore(t, memiavl.Options{})
	held, err := dst.CacheMultiStoreWithVersion(0)
	require.NoError(t, err)

	r := protoio.NewDelimitedReader(&buf, 1<<20)
	defer r.Close()
	_, err = dst.Restore(uint64(cid.Version), 1, r)
	require.NoError(t, err)

	require.Equal(t, cid, dst.LastCommitID())
	res, err := dst.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Prove: true})
	require.NoError(t, err)
	require.NoError(t, rootmulti.DefaultProofRuntime().VerifyValue(res.ProofOps, cid.Hash, "/"+testStoreName+"/k", []byte("v2")))
	require.Nil(t, held.GetKVStore(dstKey).Get([]byte("k")))
	require.NoError(t, held.(io.Closer).Close())
}

func TestUnclosedQueryStoreIsReleased(t *testing.T) {
	rs, key := newLoadedStore(t, memiavl.Options{})
	rs.GetKVStore(key).Set([]byte("k"), []byte("v"))
	rs.Commit()
	refs := func() int64 { return rs.querySnapshot.Load().refs.Load() }

	// the SDK can drop a query store without Close; an iterator from it must still pin the db.
	it := func() types.Iterator {
		cms, err := rs.CacheMultiStoreWithVersion(0)
		require.NoError(t, err)
		return cms.GetKVStore(key).Iterator(nil, nil)
	}()
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	require.EqualValues(t, 2, refs())
	require.Equal(t, []byte("v"), it.Value())
	require.NoError(t, it.Close())
	it = nil

	require.Eventually(t, func() bool {
		runtime.GC()
		return refs() == 1
	}, 5*time.Second, 10*time.Millisecond, "an unreachable unclosed store must release its reference")
}

func TestPinnedStoreKeepsCloserReachable(t *testing.T) {
	testCases := []struct {
		name   string
		derive func(s pinnedStore) any
	}{
		{"Iterator", func(s pinnedStore) any { return s.Iterator(nil, nil) }},
		{"ReverseIterator", func(s pinnedStore) any { return s.ReverseIterator(nil, nil) }},
		{"CacheWrap", func(s pinnedStore) any { return s.CacheWrap() }},
		{"CacheWrapWithTrace", func(s pinnedStore) any { return s.CacheWrapWithTrace(nil, nil) }},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var released atomic.Bool
			derived := func() any {
				closer := newQueryCloser(func() error { released.Store(true); return nil }, log.NewNopLogger())
				return tc.derive(pinnedStore{Store: memiavlstore.New(memiavl.New(0), log.NewNopLogger()), pin: closer})
			}()
			for i := 0; i < 3; i++ {
				runtime.GC()
			}
			require.False(t, released.Load(), "a live derived store or iterator must keep the db")
			runtime.KeepAlive(derived)

			require.Eventually(t, func() bool {
				runtime.GC()
				return released.Load()
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestSetInitialVersionRefreshesQuerySnapshot(t *testing.T) {
	rs, _ := newLoadedStore(t, memiavl.Options{})

	require.NoError(t, rs.SetInitialVersion(5))
	require.Equal(t, int64(5), rs.EarliestVersion())

	commitID := rs.Commit()
	require.Equal(t, int64(5), commitID.Version)
	require.Equal(t, int64(5), rs.LatestVersion())
}

func TestReadPathsReleaseQuerySnapshot(t *testing.T) {
	rs, key := newLoadedStore(t, memiavl.Options{})
	for _, v := range []string{"v1", "v2"} {
		rs.GetKVStore(key).Set([]byte("k"), []byte(v))
		rs.Commit()
	}

	query := func(height int64, prove bool) error {
		_, err := rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Height: height, Prove: prove})
		return err
	}
	cacheMultiStore := func(version int64) error {
		cms, err := rs.CacheMultiStoreWithVersion(version)
		if err != nil {
			return err
		}
		closer := cms.(io.Closer)
		// a second Close must not release the snapshot again.
		return stderrors.Join(closer.Close(), closer.Close())
	}
	testCases := []struct {
		name    string
		read    func() error
		wantErr bool
	}{
		{"Query latest", func() error { return query(0, false) }, false},
		{"Query latest height with proof", func() error { return query(2, true) }, false},
		{"Query historical height with proof", func() error { return query(1, true) }, false},
		{"Query negative height", func() error { return query(-1, false) }, true},
		{"Query future height", func() error { return query(5, false) }, true},
		{"CacheMultiStoreWithVersion latest", func() error { return cacheMultiStore(0) }, false},
		{"CacheMultiStoreWithVersion latest height", func() error { return cacheMultiStore(2) }, false},
		{"CacheMultiStoreWithVersion historical height", func() error { return cacheMultiStore(1) }, false},
		{"CacheMultiStoreWithVersion negative height", func() error { return cacheMultiStore(-1) }, true},
		{"EarliestVersion", func() error { rs.EarliestVersion(); return nil }, false},
		{"LatestVersion", func() error { rs.LatestVersion(); return nil }, false},
		{"LastCommitID", func() error { rs.LastCommitID(); return nil }, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.read()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			// a leaked reference pins the snapshot's mmap and files after later reloads.
			require.EqualValues(t, 1, rs.querySnapshot.Load().refs.Load(), "only the publish slot may hold it")
		})
	}

	// the SDK's query context passes the latest height explicitly; it must read the snapshot, not load from disk.
	latest := rs.LatestVersion()
	cms, err := rs.CacheMultiStoreWithVersion(latest)
	require.NoError(t, err)
	require.EqualValues(t, 2, rs.querySnapshot.Load().refs.Load())
	require.NoError(t, cms.(io.Closer).Close())
	cached := len(rs.historicalDBCache.entries)
	require.NoError(t, query(latest, true))
	require.Len(t, rs.historicalDBCache.entries, cached)
}

func TestAcquireQuerySnapshotSkipsReleasedSnapshot(t *testing.T) {
	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	// a reader can load a snapshot from the slot just as its last reference goes.
	rs.querySnapshot.Store(&querySnapshot{})

	acquired := make(chan *querySnapshot, 1)
	go func() { acquired <- rs.acquireQuerySnapshot() }()
	select {
	case <-acquired:
		t.Fatal("acquired a released snapshot")
	case <-time.After(20 * time.Millisecond):
	}

	live := newQuerySnapshot(nil, &types.CommitInfo{}, nil)
	rs.querySnapshot.Store(live)
	require.Same(t, live, <-acquired)
	require.EqualValues(t, 2, live.refs.Load())
}

func TestHeldQuerySnapshotOutlivesItsGeneration(t *testing.T) {
	testCases := []struct {
		name     string
		malleate func(t *testing.T, rs *Store, key types.StoreKey)
	}{
		{
			// the DB unmaps a generation at the second reload after retiring it.
			name: "two snapshot reloads",
			malleate: func(t *testing.T, rs *Store, key types.StoreKey) {
				t.Helper()
				for i := 0; i < 2; i++ {
					rs.GetKVStore(key).Set([]byte(fmt.Sprintf("n%d", i)), []byte("v"))
					rs.Commit()
					rewriteAndReload(t, rs)
				}
			},
		},
		{
			name: "store close",
			malleate: func(t *testing.T, rs *Store, _ types.StoreKey) {
				t.Helper()
				require.NoError(t, rs.Close())
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rs, key := newLoadedStore(t, memiavl.Options{})

			const numKeys = 20
			for i := 0; i < numKeys; i++ {
				rs.GetKVStore(key).Set([]byte(fmt.Sprintf("k%02d", i)), []byte("v"))
			}
			rs.Commit()
			// publish a snapshot whose trees read from the mmap'd snapshot files.
			rewriteAndReload(t, rs)
			rs.Commit()

			cms, err := rs.CacheMultiStoreWithVersion(0)
			require.NoError(t, err)
			snap := rs.querySnapshot.Load()

			tc.malleate(t, rs, key)

			it := cms.GetKVStore(key).Iterator([]byte("k"), []byte("l"))
			n := 0
			for ; it.Valid(); it.Next() {
				n++
			}
			require.NoError(t, it.Close())
			require.Equal(t, numKeys, n)

			require.EqualValues(t, 1, snap.refs.Load(), "only the reader should hold it")
			require.NoError(t, cms.(io.Closer).Close())
			require.Zero(t, snap.refs.Load())
		})
	}
}

// closeDB runs on the state-sync goroutine while ABCI queries keep arriving.
func TestReadPathsRaceAgainstCloseAndReload(t *testing.T) {
	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	key := types.NewKVStoreKey(testStoreName)
	rs.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	tkey := types.NewTransientStoreKey("transient")
	rs.MountStoreWithDB(tkey, types.StoreTypeTransient, nil)
	require.NoError(t, rs.LoadLatestVersion())
	rs.GetKVStore(key).Set([]byte("k"), []byte("v"))
	historical := rs.Commit().Version
	rs.GetKVStore(key).Set([]byte("k"), []byte("w"))
	rs.Commit()
	require.NoError(t, rs.db.WaitAsyncCommit())

	done := make(chan struct{})
	var wg sync.WaitGroup
	readers := []func(){
		func() { rs.LatestVersion() },
		func() { rs.EarliestVersion() },
		func() { rs.LastCommitID() },
		func() { _, _ = rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k")}) },
		func() {
			if cms, err := rs.CacheMultiStoreWithVersion(0); err == nil {
				_ = cms.(io.Closer).Close()
			}
		},
		func() {
			if cms, err := rs.CacheMultiStoreWithVersion(historical); err == nil {
				_ = cms.(io.Closer).Close()
			}
		},
	}
	for _, read := range readers {
		wg.Add(1)
		go func(read func()) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					read()
				}
			}
		}(read)
	}

	for i := 0; i < 10; i++ {
		require.NoError(t, rs.LoadLatestVersion())
	}
	// query stores carry the reloaded transient store.
	for _, version := range []int64{0, historical} {
		cms, err := rs.CacheMultiStoreWithVersion(version)
		require.NoError(t, err)
		require.NotPanics(t, func() { cms.GetKVStore(tkey) })
		require.NoError(t, cms.(io.Closer).Close())
	}
	require.NoError(t, rs.closeDBForReload())
	close(done)
	wg.Wait()
	require.NoError(t, rs.Close())
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

func TestRestoreRejectsBranchNodeBeforeLeaves(t *testing.T) {
	rs := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)

	var buf bytes.Buffer
	w := protoio.NewDelimitedWriter(&buf)
	require.NoError(t, w.WriteMsg(&snapshottypes.SnapshotItem{
		Item: &snapshottypes.SnapshotItem_Store{
			Store: &snapshottypes.SnapshotStoreItem{Name: "test"},
		},
	}))
	require.NoError(t, w.WriteMsg(&snapshottypes.SnapshotItem{
		Item: &snapshottypes.SnapshotItem_IAVL{
			IAVL: &snapshottypes.SnapshotIAVLItem{Key: []byte("k"), Height: 1, Version: 1},
		},
	}))
	require.NoError(t, w.Close())

	r := protoio.NewDelimitedReader(&buf, 1<<20)
	defer r.Close()

	_, err := rs.restore(1, 1, r)
	require.ErrorContains(t, err, "invalid node structure")
}

func TestLatestHeightQueryRaceAgainstCommit(t *testing.T) {
	store, key := newLoadedStore(t, memiavl.Options{})

	// enough keys for multi-level paths, so each commit rewrites shared inner nodes.
	commit := func(i int) {
		kv := store.GetKVStore(key)
		for j := 0; j < 16; j++ {
			kv.Set([]byte(fmt.Sprintf("k%02d", j)), []byte{byte(i)})
		}
		store.Commit()
	}
	// a proof query on an empty tree panics in memiavlstore, so start readers on a populated one.
	commit(0)

	const commits = 30
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 1; i <= commits; i++ {
			commit(i)
		}
	}()

	readers := []func(){
		// wraps live rs.stores: only construction is safe here, not a read.
		func() { store.CacheMultiStore() },
		func() {
			cms, err := store.CacheMultiStoreWithVersion(0)
			if err != nil {
				return
			}
			kv := cms.GetKVStore(key)
			_ = kv.Get([]byte("k07"))
			it := kv.Iterator(nil, nil)
			for ; it.Valid(); it.Next() {
				_ = it.Value()
			}
			_ = it.Close()
			_ = cms.(io.Closer).Close()
		},
		func() {
			_, _ = store.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k07")})
		},
		func() {
			_, _ = store.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k07"), Prove: true})
		},
		func() {
			_, _ = store.Query(&types.RequestQuery{Path: "/" + testStoreName + "/subspace", Data: []byte("k")})
		},
		func() { store.LatestVersion() },
		func() { store.EarliestVersion() },
	}

	for _, read := range readers {
		wg.Add(1)
		go func(read func()) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					read()
				}
			}
		}(read)
	}

	wg.Wait()
}

func TestHistoricalQueryAfterRollbackDoesNotServeStaleCache(t *testing.T) {
	rs, key := newLoadedStore(t, memiavl.Options{})

	rs.GetKVStore(key).Set([]byte("k"), []byte("v1"))
	rs.Commit()
	rs.GetKVStore(key).Set([]byte("k"), []byte("v2"))
	rs.Commit()
	// Commit past the queried height so the query below goes through the historical
	// DB cache rather than the latest-height query snapshot.
	rs.GetKVStore(key).Set([]byte("k"), []byte("v3"))
	rs.Commit()

	// Query mutates req.Path, so each call needs its own request.
	newQuery := func() *types.RequestQuery {
		return &types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Height: 2, Prove: true}
	}
	res, err := rs.Query(newQuery())
	require.NoError(t, err)
	require.Equal(t, []byte("v2"), res.Value)

	require.NoError(t, rs.RollbackToVersion(1))

	// the cached version-2 db maps files the rollback discarded.
	_, err = rs.Query(newQuery())
	require.Error(t, err)

	// the rollback publishes its target as the latest height.
	require.Equal(t, int64(1), rs.LatestVersion())
	res, err = rs.Query(&types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k")})
	require.NoError(t, err)
	require.Equal(t, []byte("v1"), res.Value)
}

func TestRollbackToVersionAcrossStoreUpgrade(t *testing.T) {
	for _, tc := range storeUpgradeCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			open := func(names []string) *Store {
				rs := NewStore(dir, log.NewNopLogger(), false, false, TestAppChainID)
				for _, name := range names {
					rs.MountStoreWithDB(types.NewKVStoreKey(name), types.StoreTypeIAVL, nil)
				}
				return rs
			}

			rs := open(tc.initialStores)
			require.NoError(t, rs.LoadLatestVersion())
			rs.Commit()
			require.NoError(t, rs.Close())

			rs = open(tc.mountedStores)
			require.NoError(t, rs.LoadLatestVersionAndUpgrade(tc.upgrades))
			rs.Commit()
			require.NoError(t, rs.Close())

			// the rollback CLI loads the app at the latest height, so the mounted
			// stores match the upgraded tree set, not the one at the target.
			rs = open(tc.mountedStores)
			require.NoError(t, rs.LoadLatestVersion())
			require.NoError(t, rs.RollbackToVersion(1))
			require.Equal(t, int64(1), rs.LastCommitID().Version)
			require.NoError(t, rs.Close())

			// the next start runs the upgrade again.
			rs = open(tc.mountedStores)
			require.NoError(t, rs.LoadLatestVersionAndUpgrade(tc.upgrades))
			require.Equal(t, int64(1), rs.LastCommitID().Version)
			require.NoError(t, rs.Close())
		})
	}
}

// WorkingHash flushes the next block into the live tree before Commit.
func TestLatestHeightReadsIgnoreUncommittedWrites(t *testing.T) {
	store, versions := newTestStore(t, 1)
	defer store.Close()
	key := store.keysByName[testStoreName]

	store.GetKVStore(key).Set([]byte("k"), []byte("dirty"))
	store.WorkingHash()

	newQuery := func(height int64) *types.RequestQuery {
		return &types.RequestQuery{Path: "/" + testStoreName + "/key", Data: []byte("k"), Height: height}
	}
	res, err := store.Query(newQuery(0))
	require.NoError(t, err)
	require.Equal(t, []byte{0}, res.Value)
	require.Equal(t, versions[0], res.Height)

	for _, v := range []int64{0, versions[0]} {
		cms, err := store.CacheMultiStoreWithVersion(v)
		require.NoError(t, err)
		require.Equal(t, []byte{0}, cms.GetKVStore(key).Get([]byte("k")))
		require.NoError(t, cms.(io.Closer).Close())
	}

	cid := store.Commit()
	res, err = store.Query(newQuery(0))
	require.NoError(t, err)
	require.Equal(t, []byte("dirty"), res.Value)
	require.Equal(t, cid.Version, res.Height)
	require.Equal(t, cid.Version, store.LatestVersion())
}
