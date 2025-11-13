package memiavl

import (
	"fmt"
	"testing"
)

func BenchmarkApplyChangeSet100(b *testing.B) {
	benchmarkApplyChangeSet(b, 100)
}

func BenchmarkApplyChangeSet1000(b *testing.B) {
	benchmarkApplyChangeSet(b, 1000)
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
		for i, name := range storeNames {
			if err := db.applyChangeSet(name, changeSets[i]); err != nil {
				b.Fatal(err)
			}
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
