package memiavl

import (
	"fmt"
	"sync"
	"testing"
)

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

// BenchmarkConcurrentReads tests the RWMutex optimization for concurrent read operations
func BenchmarkConcurrentReads(b *testing.B) {
	dir := b.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"store1", "store2", "store3"},
		CacheSize:       1000,
	}, "test-chain")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	// Populate with some data
	changeSets := make([]*NamedChangeSet, 3)
	for i := 0; i < 3; i++ {
		pairs := make([]*KVPair, 100)
		for j := 0; j < 100; j++ {
			pairs[j] = &KVPair{
				Key:   []byte(fmt.Sprintf("key-%d-%d", i, j)),
				Value: []byte(fmt.Sprintf("value-%d-%d", i, j)),
			}
		}
		changeSets[i] = &NamedChangeSet{
			Name:      fmt.Sprintf("store%d", i+1),
			Changeset: ChangeSet{Pairs: pairs},
		}
	}
	if err := db.ApplyChangeSets(changeSets); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Commit(); err != nil {
		b.Fatal(err)
	}

	// Benchmark concurrent reads
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Mix of different read operations
			_ = db.Version()
			_ = db.LastCommitInfo()
			_ = db.TreeByName("store1")
			_ = db.WorkingCommitInfo()
		}
	})
}

func BenchmarkConcurrentReadsVsWrites(b *testing.B) {
	dir := b.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"store1"},
		CacheSize:       1000,
	}, "test-chain")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	// Start a background writer
	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		counter := 0
		for {
			select {
			case <-stopWriter:
				return
			default:
				changeSets := []*NamedChangeSet{{
					Name: "store1",
					Changeset: ChangeSet{Pairs: []*KVPair{{
						Key:   []byte(fmt.Sprintf("key-%d", counter)),
						Value: []byte(fmt.Sprintf("value-%d", counter)),
					}}},
				}}
				_ = db.ApplyChangeSets(changeSets)
				_, _ = db.Commit()
				counter++
			}
		}
	}()

	// Benchmark concurrent reads while writes are happening
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = db.Version()
			_ = db.LastCommitInfo()
		}
	})
	b.StopTimer()

	close(stopWriter)
	<-writerDone
}

func BenchmarkIteratorAllocation(b *testing.B) {
	tree := New(0)

	// Create a tree with depth to test stack allocation
	pairs := make([]*KVPair, 1000)
	for i := 0; i < 1000; i++ {
		pairs[i] = &KVPair{
			Key:   []byte(fmt.Sprintf("key-%04d", i)),
			Value: []byte(fmt.Sprintf("value-%d", i)),
		}
	}
	tree.ApplyChangeSet(ChangeSet{Pairs: pairs})
	_, _, _ = tree.SaveVersion(true)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		iter := tree.Iterator(nil, nil, true)
		for iter.Valid() {
			_ = iter.Key()
			_ = iter.Value()
			iter.Next()
		}
	}
}

func BenchmarkDBVersionConcurrent(b *testing.B) {
	dir := b.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"store1"},
	}, "test-chain")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = db.Version()
		}
	})
}

func BenchmarkTreeByNameConcurrent(b *testing.B) {
	dir := b.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"store1", "store2", "store3"},
	}, "test-chain")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		stores := []string{"store1", "store2", "store3"}
		idx := 0
		for pb.Next() {
			_ = db.TreeByName(stores[idx%3])
			idx++
		}
	})
}

func BenchmarkReadWriteContention(b *testing.B) {
	for _, readRatio := range []int{50, 80, 95, 99} {
		b.Run(fmt.Sprintf("reads=%d%%", readRatio), func(b *testing.B) {
			dir := b.TempDir()
			db, err := Load(dir, Options{
				CreateIfMissing: true,
				InitialStores:   []string{"store1"},
				CacheSize:       1000,
			}, "test-chain")
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				counter := 0
				for pb.Next() {
					counter++
					if counter%100 < readRatio {
						_ = db.Version()
						_ = db.LastCommitInfo()
					} else {
						changeSets := []*NamedChangeSet{{
							Name: "store1",
							Changeset: ChangeSet{Pairs: []*KVPair{{
								Key:   []byte(fmt.Sprintf("key-%d", counter)),
								Value: []byte(fmt.Sprintf("value-%d", counter)),
							}}},
						}}
						_ = db.ApplyChangeSets(changeSets)
						_, _ = db.Commit()
					}
				}
			})
		})
	}
}

func BenchmarkCopyContention(b *testing.B) {
	dir := b.TempDir()
	db, err := Load(dir, Options{
		CreateIfMissing: true,
		InitialStores:   []string{"store1"},
		CacheSize:       1000,
	}, "test-chain")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	changeSets := []*NamedChangeSet{{
		Name: "store1",
		Changeset: ChangeSet{Pairs: []*KVPair{
			{Key: []byte("key1"), Value: []byte("value1")},
			{Key: []byte("key2"), Value: []byte("value2")},
		}},
	}}
	if err := db.ApplyChangeSets(changeSets); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Commit(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	// Test concurrent Copy operations (should be serialized)
	var wg sync.WaitGroup
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := db.Copy()
			_ = copy
		}()
	}
	wg.Wait()
}
