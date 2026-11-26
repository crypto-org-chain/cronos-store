package tsrocksdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Domain must report the caller's unprefixed bounds, not the prefixed range the
// iterator rewrites start/end to internally.
func TestIteratorDomain(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	cases := []struct {
		name       string
		reverse    bool
		start, end []byte
	}{
		{name: "nil bounds"},
		{name: "nil bounds reverse", reverse: true},
		{name: "explicit bounds", start: []byte("aaa"), end: []byte("zzz")},
		{name: "explicit end only", end: []byte("zzz")},
		{name: "explicit start only", start: []byte("aaa")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newIterator := store.IteratorAtVersion
			if tc.reverse {
				newIterator = store.ReverseIteratorAtVersion
			}
			itr, err := newIterator(testStoreKey, tc.start, tc.end, nil)
			require.NoError(t, err)
			defer itr.Close()

			start, end := itr.Domain()
			require.Equal(t, tc.start, start)
			require.Equal(t, tc.end, end)
		})
	}
}
