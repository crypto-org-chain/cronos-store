package memiavl

import (
	"fmt"
	"testing"
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
