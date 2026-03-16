package rootmulti

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crypto-org-chain/cronos-store/memiavl"
	"cosmossdk.io/log"
	"cosmossdk.io/store/types"
)

const TestAppChainID = "test_chain"

func TestLastCommitID(t *testing.T) {
	store := NewStore(t.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	require.Equal(t, types.CommitID{}, store.LastCommitID())
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
		v := v
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
			require.NotNil(t, entry.db)
			cache.release(entry)
		}(i)
	}

	wg.Wait()

	// Clean up the cache created for this test.
	require.NoError(t, cache.close())
}
