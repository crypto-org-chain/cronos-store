package client

import (
	"bytes"
	"testing"

	protoio "github.com/cosmos/gogoproto/io"
	"github.com/crypto-org-chain/cronos-store/versiondb"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/store/v2/snapshots/types"
)

// TestReadSnapshotEntriesSkipsInternalNodes is a regression test for the
// restore-versiondb fix: only IAVL leaf nodes (Height == 0) carry user data,
// so internal nodes must be dropped.
func TestReadSnapshotEntriesSkipsInternalNodes(t *testing.T) {
	var buf bytes.Buffer
	w := protoio.NewDelimitedWriter(&buf)

	write := func(item *types.SnapshotItem) {
		require.NoError(t, w.WriteMsg(item))
	}

	write(&types.SnapshotItem{
		Item: &types.SnapshotItem_Store{
			Store: &types.SnapshotStoreItem{Name: "bank"},
		},
	})
	write(&types.SnapshotItem{
		Item: &types.SnapshotItem_IAVL{
			IAVL: &types.SnapshotIAVLItem{
				Key: []byte("k1"), Value: []byte("v1"), Height: 0, Version: 1,
			},
		},
	})
	write(&types.SnapshotItem{
		Item: &types.SnapshotItem_IAVL{
			IAVL: &types.SnapshotIAVLItem{
				Key: []byte("k2"), Value: []byte("v2"), Height: 0, Version: 1,
			},
		},
	})
	// Internal node whose pivot key collides with the "k1" leaf above.
	// Without the Height != 0 filter this would clobber v1 with nil.
	write(&types.SnapshotItem{
		Item: &types.SnapshotItem_IAVL{
			IAVL: &types.SnapshotIAVLItem{
				Key: []byte("k1"), Value: nil, Height: 1, Version: 1,
			},
		},
	})

	require.NoError(t, w.Close())

	r := protoio.NewDelimitedReader(&buf, 1<<20)
	defer r.Close()

	ch := make(chan versiondb.ImportEntry, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(ch)
		errCh <- readSnapshotEntries(r, ch)
	}()

	var got []versiondb.ImportEntry
	for e := range ch {
		got = append(got, e)
	}
	require.NoError(t, <-errCh)

	require.Equal(t, []versiondb.ImportEntry{
		{StoreKey: "bank", Key: []byte("k1"), Value: []byte("v1")},
		{StoreKey: "bank", Key: []byte("k2"), Value: []byte("v2")},
	}, got)
}
