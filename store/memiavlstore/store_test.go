package memiavlstore

import (
	"runtime"
	"testing"
	"time"

	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/stretchr/testify/require"

	log "cosmossdk.io/log/v2"
)

func newViewTestDB(t *testing.T) *memiavl.DB {
	t.Helper()
	db, err := memiavl.Load(t.TempDir(), memiavl.Options{CreateIfMissing: true, InitialStores: []string{"s"}}, "test_chain")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.ApplyChangeSets([]*memiavl.NamedChangeSet{{
		Name:      "s",
		Changeset: memiavl.ChangeSet{Pairs: []*memiavl.KVPair{{Key: []byte("k"), Value: []byte("v")}}},
	}}))
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
			db := newViewTestDB(t)
			cp := db.CopyWithCacheSize(0)
			done := make(chan struct{})
			var mount Mount
			mount.Swap(newView(cp, func() {
				close(done)
				_ = cp.Close()
			}))
			st := NewMounted(&mount, "s", log.NewNopLogger())
			require.Equal(t, []byte("v"), st.Get([]byte("k")))

			held := tc.hold(st)
			mount.Swap(nil)
			require.Nil(t, st.Get([]byte("k")), "a store with no view reads empty")
			if held != nil {
				require.False(t, released(done, 100*time.Millisecond), "a view in use must not be released")
				runtime.KeepAlive(held)
			}
			require.True(t, released(done, 5*time.Second), "an unreachable view must be released")
		})
	}
}
