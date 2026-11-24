package memiavlstore

import (
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/stretchr/testify/require"

	log "cosmossdk.io/log/v2"
)

// newViewTestDB commits key "k" in each store, with the store's name as the value.
func newViewTestDB(t *testing.T, stores ...string) *memiavl.DB {
	t.Helper()
	db, err := memiavl.Load(t.TempDir(), memiavl.Options{CreateIfMissing: true, InitialStores: stores}, "test_chain")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	changeSets := make([]*memiavl.NamedChangeSet, len(stores))
	for i, name := range stores {
		changeSets[i] = &memiavl.NamedChangeSet{
			Name:      name,
			Changeset: memiavl.ChangeSet{Pairs: []*memiavl.KVPair{{Key: []byte("k"), Value: []byte(name)}}},
		}
	}
	require.NoError(t, db.ApplyChangeSets(changeSets))
	_, err = db.Commit()
	require.NoError(t, err)
	return db
}

// released reports whether release ran, collecting garbage until it does or the wait ends.
func released(done <-chan struct{}, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-done:
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

func TestMountedViewRelease(t *testing.T) {
	testCases := []struct {
		name string
		// hold returns something that keeps reading the view after it is swapped out.
		hold func(st *Store) any
	}{
		{name: "unreachable", hold: func(*Store) any { return nil }},
		{name: "iterator", hold: func(st *Store) any { return st.Iterator(nil, nil) }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			db := newViewTestDB(t, "s")
			cp := db.CopyWithCacheSize(0)
			done := make(chan struct{})
			var mount Mount
			mount.Publish(newView(cp, func() {
				close(done)
				_ = cp.Close()
			}))
			st := NewMounted(&mount, "s", log.NewNopLogger())
			require.Equal(t, []byte("s"), st.Get([]byte("k")))

			held := tc.hold(st)
			mount.Publish(nil)
			require.Nil(t, st.Get([]byte("k")), "a store with no view reads empty")
			if held != nil {
				require.False(t, released(done, 100*time.Millisecond), "a view in use must not be released")
				runtime.KeepAlive(held)
			}
			require.True(t, released(done, 5*time.Second), "an unreachable view must be released")
		})
	}
}

func TestMountedStoreFollowsItsTreeAcrossViews(t *testing.T) {
	testCases := []struct {
		name string
		// the stores of each view published in turn; "s" is read after every publish.
		views [][]string
	}{
		{name: "same position", views: [][]string{{"a", "s"}, {"a", "s"}}},
		{name: "moved forward", views: [][]string{{"a", "s"}, {"s"}}},
		{name: "moved back", views: [][]string{{"s"}, {"a", "b", "s"}}},
		{name: "another store took its position", views: [][]string{{"a", "s"}, {"a", "b", "s"}}},
		{name: "removed then added back", views: [][]string{{"a", "s"}, {"a", "b"}, {"a", "s"}}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var mount Mount
			st := NewMounted(&mount, "s", log.NewNopLogger())
			for _, stores := range tc.views {
				mount.Publish(NewView(newViewTestDB(t, stores...).CopyWithCacheSize(0), log.NewNopLogger()))
				var expected []byte
				if slices.Contains(stores, "s") {
					expected = []byte("s")
				}
				require.Equal(t, expected, st.Get([]byte("k")))
			}
			mount.Publish(nil)
		})
	}
}

func TestSetTreeOnMountedStorePanics(t *testing.T) {
	st := NewMounted(&Mount{}, "s", log.NewNopLogger())
	require.Panics(t, func() { st.SetTree(memiavl.New(0)) })
}
