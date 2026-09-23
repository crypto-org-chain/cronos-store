package memiavl

import "testing"

func BenchmarkIteratorRepeatedKeyAccess(b *testing.B) {
	tmpDir := b.TempDir()
	tree := New(0)
	for _, changes := range ChangeSets[:6] {
		tree.ApplyChangeSet(changes)
		if _, _, err := tree.SaveVersion(true); err != nil {
			b.Fatal(err)
		}
	}
	if err := tree.WriteSnapshot(tmpDir); err != nil {
		b.Fatal(err)
	}

	snapshot, err := OpenSnapshot(tmpDir)
	if err != nil {
		b.Fatal(err)
	}
	ptree := NewFromSnapshot(snapshot, false, 0)
	b.Cleanup(func() { ptree.Close() })

	b.ResetTimer()
	b.ReportAllocs()
	var sink int
	for i := 0; i < b.N; i++ {
		iter := ptree.Iterator(nil, nil, true)
		for ; iter.Valid(); iter.Next() {
			sink += len(iter.Key()) + len(iter.Key()) + len(iter.Key()) + len(iter.Value())
		}
		if err := iter.Close(); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}
