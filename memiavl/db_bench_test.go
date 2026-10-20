package memiavl

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkCommit(b *testing.B) {
	for _, asyncCommit := range []bool{false, true} {
		b.Run(fmt.Sprintf("asyncCommit=%v", asyncCommit), func(b *testing.B) {
			benchmarkCommit(b, asyncCommit)
		})
	}
}

func benchmarkCommit(b *testing.B, asyncCommit bool) {
	b.Helper()

	db, err := Load(b.TempDir(), Options{
		CreateIfMissing:   true,
		InitialStores:     []string{"store"},
		AsyncCommitBuffer: asyncCommitBufferFor(asyncCommit),
	}, "bench_chain")
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}()

	cs := []*NamedChangeSet{{
		Name:      "store",
		Changeset: ChangeSet{Pairs: []*KVPair{{Key: []byte("key"), Value: []byte("value")}}},
	}}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := db.ApplyChangeSets(cs); err != nil {
			b.Fatal(err)
		}
		if _, err := db.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplyChangeSet100(b *testing.B) {
	benchmarkApplyChangeSet(b, 100)
}

func BenchmarkApplyChangeSet1000(b *testing.B) {
	benchmarkApplyChangeSet(b, 1000)
}

func BenchmarkApplyChangeSets100(b *testing.B) {
	benchmarkApplyChangeSets(b, 100)
}

func BenchmarkApplyChangeSets1000(b *testing.B) {
	benchmarkApplyChangeSets(b, 1000)
}

func benchmarkApplyChangeSet(b *testing.B, storeCount int) {
	b.Helper()
	db := newBenchmarkDB(storeCount)
	storeNames := make([]string, storeCount)
	changeSets := make([]ChangeSet, storeCount)
	for i := 0; i < storeCount; i++ {
		name := fmt.Sprintf("store-%d", i)
		storeNames[i] = name
		changeSets[i] = ChangeSet{
			Pairs: []*KVPair{{
				Key:   []byte(fmt.Sprintf("key-%d", i)),
				Value: []byte("value"),
			}},
		}
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		db.pendingLog = WALEntry{}
		if db.cachedPendingChangesets != nil {
			clear(db.cachedPendingChangesets)
		}
		for i, name := range storeNames {
			if err := db.applyChangeSet(name, changeSets[i]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func benchmarkApplyChangeSets(b *testing.B, storeCount int) {
	b.Helper()
	db := newBenchmarkDB(storeCount)
	changeSets := make([]*NamedChangeSet, storeCount)
	for i := 0; i < storeCount; i++ {
		name := fmt.Sprintf("store-%d", i)
		changeSets[i] = &NamedChangeSet{
			Name: name,
			Changeset: ChangeSet{
				Pairs: []*KVPair{{
					Key:   []byte(fmt.Sprintf("key-%d", i)),
					Value: []byte("value"),
				}},
			},
		}
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		db.pendingLog = WALEntry{}
		if db.cachedPendingChangesets != nil {
			clear(db.cachedPendingChangesets)
		}
		if err := db.ApplyChangeSets(changeSets); err != nil {
			b.Fatal(err)
		}
	}
}

func newBenchmarkDB(storeCount int) *DB {
	mtree := NewEmptyMultiTree(0, 0, "")
	upgrades := make([]*TreeNameUpgrade, storeCount)
	for i := 0; i < storeCount; i++ {
		upgrades[i] = &TreeNameUpgrade{Name: fmt.Sprintf("store-%d", i)}
	}
	if err := mtree.ApplyUpgrades(upgrades); err != nil {
		panic(err)
	}
	return &DB{
		MultiTree: *mtree,
		logger:    NewNopLogger(),
	}
}

const queryCopyBenchKeys = 200_000

// newQueryCopyBenchDB returns a DB whose single store "s" reads from its
// mmap'd snapshot, like a published query copy between rewrites.
func newQueryCopyBenchDB(b *testing.B) *DB {
	b.Helper()
	db, err := Load(b.TempDir(), Options{CreateIfMissing: true, InitialStores: []string{"s"}}, TestAppChainID)
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	var cs ChangeSet
	for i := 0; i < queryCopyBenchKeys; i++ {
		cs.Pairs = append(cs.Pairs, &KVPair{Key: benchKey(i), Value: make([]byte, 64)})
	}
	require.NoError(b, db.ApplyChangeSets([]*NamedChangeSet{{Name: "s", Changeset: cs}}))
	_, err = db.Commit()
	require.NoError(b, err)
	require.NoError(b, db.RewriteSnapshot())
	require.NoError(b, db.Reload())
	return db
}

// BenchmarkQueryCopyGet reads a published query copy from parallel goroutines,
// by cache size and zero-copy setting, over hot key sets of growing size. Each
// sub-benchmark's cache starts cold, like one block's.
func BenchmarkQueryCopyGet(b *testing.B) {
	db := newQueryCopyBenchDB(b)
	for _, cacheSize := range []int{0, 1000} {
		for _, zeroCopy := range []bool{false, true} {
			for _, hot := range []int{16, 1_000, 16_000, queryCopyBenchKeys} {
				b.Run(fmt.Sprintf("cache=%d/zerocopy=%v/hot=%d", cacheSize, zeroCopy, hot), func(b *testing.B) {
					cp := db.CopyWithCacheSize(cacheSize)
					defer cp.Close()
					cp.SetZeroCopy(zeroCopy)
					tree := cp.TreeByName("s")
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						rng := rand.New(rand.NewSource(rand.Int63()))
						for pb.Next() {
							tree.Get(benchKey(rng.Intn(hot)))
						}
					})
				})
			}
		}
	}
}

// BenchmarkQueryCopyIterate scans 100 keys of a published query copy from
// parallel goroutines, by zero-copy setting.
func BenchmarkQueryCopyIterate(b *testing.B) {
	db := newQueryCopyBenchDB(b)
	for _, zeroCopy := range []bool{false, true} {
		b.Run(fmt.Sprintf("zerocopy=%v", zeroCopy), func(b *testing.B) {
			cp := db.CopyWithCacheSize(0)
			defer cp.Close()
			cp.SetZeroCopy(zeroCopy)
			tree := cp.TreeByName("s")
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewSource(rand.Int63()))
				for pb.Next() {
					it := tree.Iterator(benchKey(rng.Intn(queryCopyBenchKeys-100)), nil, true)
					for i := 0; i < 100 && it.Valid(); i++ {
						_, _ = it.Key(), it.Value()
						it.Next()
					}
					_ = it.Close()
				}
			})
		})
	}
}

// BenchmarkQueryCopyPublish is the per-block cost of the query copy rootmulti
// publishes, for 20 stores, with and without a cache.
func BenchmarkQueryCopyPublish(b *testing.B) {
	db := newBenchmarkDB(20)
	for _, cacheSize := range []int{0, 1000} {
		b.Run(fmt.Sprintf("cache=%d", cacheSize), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = db.CopyWithCacheSize(cacheSize).Close()
			}
		})
	}
}

func benchKey(i int) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(i)*7919)
}

// BenchmarkCommitWithPublishedCopy is one block's writes and Commit, with and
// without publishing a query copy after it as rootmulti does. A copy makes the
// next block's writes clone the in-memory nodes it shares instead of mutating them.
func BenchmarkCommitWithPublishedCopy(b *testing.B) {
	const (
		stores    = 4
		storeKeys = 50_000
	)
	names := make([]string, stores)
	for i := range names {
		names[i] = fmt.Sprintf("store-%d", i)
	}
	for _, writes := range []int{50, 1000} {
		for _, publish := range []bool{false, true} {
			b.Run(fmt.Sprintf("writes=%d/publish=%v", writes, publish), func(b *testing.B) {
				db, err := Load(b.TempDir(), Options{CreateIfMissing: true, InitialStores: names, SnapshotInterval: 1 << 30}, TestAppChainID)
				require.NoError(b, err)
				defer db.Close()
				changeSets := func(keys func(int) int, perStore int) []*NamedChangeSet {
					cs := make([]*NamedChangeSet, stores)
					for s := range cs {
						pairs := make([]*KVPair, perStore)
						for i := range pairs {
							pairs[i] = &KVPair{Key: benchKey(keys(i)), Value: make([]byte, 64)}
						}
						cs[s] = &NamedChangeSet{Name: names[s], Changeset: ChangeSet{Pairs: pairs}}
					}
					return cs
				}
				require.NoError(b, db.ApplyChangeSets(changeSets(func(i int) int { return i }, storeKeys)))
				_, err = db.Commit()
				require.NoError(b, err)
				require.NoError(b, db.RewriteSnapshot())
				require.NoError(b, db.Reload())

				rng := rand.New(rand.NewSource(1))
				var published *DB
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					require.NoError(b, db.ApplyChangeSets(changeSets(func(int) int { return rng.Intn(storeKeys) }, writes/stores)))
					_, err := db.Commit()
					require.NoError(b, err)
					if publish {
						cp := db.CopyWithCacheSize(0)
						if published != nil {
							require.NoError(b, published.Close())
						}
						published = cp
					}
				}
				b.StopTimer()
				if published != nil {
					require.NoError(b, published.Close())
				}
			})
		}
	}
}
