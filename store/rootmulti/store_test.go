package rootmulti

import (
	"bytes"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	protoio "github.com/cosmos/gogoproto/io"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/stretchr/testify/require"

	log "cosmossdk.io/log/v2"

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

func TestLoadLatestVersionAndUpgradeValidatesMemiAVLTreeMembership(t *testing.T) {
	tests := []struct {
		name          string
		initialStores []string
		mountedStores []string
		upgrades      *types.StoreUpgrades
		dataStore     string
		loadedStore   string
	}{
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

	for _, tc := range tests {
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
	// one more commit so no queried version is current, else Query would bypass historicalDBCache via rs.lastCommitInfo
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
