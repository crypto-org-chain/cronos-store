package rootmulti

import (
	"fmt"
	"testing"

	"github.com/crypto-org-chain/cronos-store/memiavl"

	log "cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

const benchNumKeys = 1000

// returns a store whose `target` version has an on-disk snapshot and is no
// longer the latest height, so queries at it go through historicalDBCache
func setupHistoricalBenchStore(b *testing.B) (*Store, int64) {
	b.Helper()

	store := NewStore(b.TempDir(), log.NewNopLogger(), false, false, TestAppChainID)
	store.SetMemIAVLOptions(memiavl.Options{
		// larger than the versions committed here, so the background rewrite
		// never races the synchronous RewriteSnapshot below
		SnapshotInterval:   10000,
		SnapshotKeepRecent: 100,
		ZeroCopy:           true, // operator opt-in; loadAtVersion must override it
		CacheSize:          0,
	})

	key := types.NewKVStoreKey(testStoreName)
	store.MountStoreWithDB(key, types.StoreTypeIAVL, nil)
	if err := store.LoadLatestVersion(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })

	kvStore := store.GetKVStore(key)
	for i := 0; i < benchNumKeys; i++ {
		kvStore.Set([]byte(fmt.Sprintf("pfx/%06d", i)), []byte(fmt.Sprintf("value-%06d-padding-padding-padding", i)))
	}
	target := store.Commit().Version
	if err := store.db.WaitAsyncCommit(); err != nil {
		b.Fatal(err)
	}
	if err := store.db.RewriteSnapshot(); err != nil {
		b.Fatal(err)
	}

	store.GetKVStore(key).Set([]byte("zzz"), []byte("v"))
	store.Commit()
	if err := store.db.WaitAsyncCommit(); err != nil {
		b.Fatal(err)
	}
	return store, target
}

// measures the copy overhead loadAtVersion's forced ZeroCopy=false adds, with
// the DB already in the LRU so the load cost is outside the measured path
func BenchmarkHistoricalQuery(b *testing.B) {
	store, target := setupHistoricalBenchStore(b)
	path := "/" + testStoreName + "/key"
	subspacePath := "/" + testStoreName + "/subspace"
	pointKey := []byte(fmt.Sprintf("pfx/%06d", benchNumKeys/2))

	if _, err := store.Query(&types.RequestQuery{Path: path, Data: pointKey, Height: target}); err != nil {
		b.Fatal(err)
	}

	b.Run("point", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, err := store.Query(&types.RequestQuery{Path: path, Data: pointKey, Height: target})
			if err != nil {
				b.Fatal(err)
			}
			if res.Value == nil {
				b.Fatal("missing value")
			}
		}
	})

	b.Run("point_prove", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, err := store.Query(&types.RequestQuery{Path: path, Data: pointKey, Height: target, Prove: true})
			if err != nil {
				b.Fatal(err)
			}
			if res.ProofOps == nil {
				b.Fatal("missing proof")
			}
		}
	})

	b.Run("subspace", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, err := store.Query(&types.RequestQuery{Path: subspacePath, Data: []byte("pfx/"), Height: target})
			if err != nil {
				b.Fatal(err)
			}
			if len(res.Value) == 0 {
				b.Fatal("empty subspace result")
			}
		}
	})
}

// the cold cost the LRU amortizes, for scale against BenchmarkHistoricalQuery
func BenchmarkHistoricalLoad(b *testing.B) {
	store, target := setupHistoricalBenchStore(b)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		db, err := loadAtVersion(store.dir, store.opts, TestAppChainID, target)
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
