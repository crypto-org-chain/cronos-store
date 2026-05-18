package client

import (
	"bytes"
	"errors"
	"testing"

	protoio "github.com/cosmos/gogoproto/io"
	proto "github.com/cosmos/gogoproto/proto"
	"github.com/crypto-org-chain/cronos-store/versiondb"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/store/snapshots/types"
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


// errReader is a protoio.Reader that returns a fixed error after n successful reads.
type errReader struct {
	inner protoio.Reader
	after int
	reads int
}

func (r *errReader) ReadMsg(msg proto.Message) error {
	if r.reads >= r.after {
		return errors.New("read error")
	}
	r.reads++
	return r.inner.ReadMsg(msg)
}

func makeSnapshotReader(items []*types.SnapshotItem) protoio.Reader {
	var buf bytes.Buffer
	w := protoio.NewDelimitedWriter(&buf)
	for _, item := range items {
		if err := w.WriteMsg(item); err != nil {
			panic(err)
		}
	}
	_ = w.Close()
	return protoio.NewDelimitedReader(&buf, 1<<20)
}


func TestReadSnapshotEntriesReturnsError(t *testing.T) {
	// Fails after the store item is read, mid-stream.
	inner := makeSnapshotReader([]*types.SnapshotItem{
		{Item: &types.SnapshotItem_Store{Store: &types.SnapshotStoreItem{Name: "bank"}}},
		{Item: &types.SnapshotItem_IAVL{IAVL: &types.SnapshotIAVLItem{Key: []byte("k1"), Value: []byte("v1"), Height: 0}}},
	})
	r := &errReader{inner: inner, after: 1}

	ch := make(chan versiondb.ImportEntry, 8)
	err := readSnapshotEntries(r, ch)
	require.ErrorContains(t, err, "read error")
}

// TestReadSnapshotEntriesErrorPropagated verifies that an error from
// readSnapshotEntries is propagated to the caller and not silently dropped.
func TestReadSnapshotEntriesErrorPropagated(t *testing.T) {
	inner := makeSnapshotReader([]*types.SnapshotItem{
		{Item: &types.SnapshotItem_Store{Store: &types.SnapshotStoreItem{Name: "bank"}}},
	})
	r := &errReader{inner: inner, after: 1}

	ch := make(chan versiondb.ImportEntry, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(ch)
		errCh <- readSnapshotEntries(r, ch)
	}()

	// drain the channel (simulates versionDB.Import)
	for range ch {
	}

	require.ErrorContains(t, <-errCh, "read error")
}
