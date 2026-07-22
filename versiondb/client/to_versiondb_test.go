package client

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmos/iavl"
	"github.com/crypto-org-chain/cronos-store/versiondb/tsrocksdb"
	"github.com/stretchr/testify/require"
)

// TestChangeSetToVersionDBCmdDurablyPersists is a regression test for the
// to-versiondb command reporting success without flushing/closing the
// store: it feeds a change set through the real command, then reopens a
// brand-new Store against the same directory (which only succeeds once the
// original RocksDB handle has actually been closed) and verifies the fed
// data is readable from that fresh handle. This is different from just
// querying versionDB in-process, which would pass even if the writes were
// only sitting in memory/OS buffers and never durably reached disk.
func TestChangeSetToVersionDBCmdDurablyPersists(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "versiondb")

	changeSet := &iavl.ChangeSet{
		Pairs: []*iavl.KVPair{
			{Key: []byte("k1"), Value: []byte("v1")},
			{Key: []byte("k2"), Value: []byte("v2")},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, WriteChangeSet(&buf, 1, changeSet))
	changeSetFile := filepath.Join(dir, "changeset.plain")
	require.NoError(t, os.WriteFile(changeSetFile, buf.Bytes(), 0o600))

	cmd := ChangeSetToVersionDBCmd()
	cmd.SetArgs([]string{dbDir, changeSetFile, "--" + flagStore, testStoreKey})
	require.NoError(t, cmd.Execute())

	// Reopening the same directory only succeeds if the command actually
	// closed its own RocksDB handle (RocksDB holds an exclusive file lock),
	// which in turn proves the command didn't just leave the store open and
	// return "success" while writes were still pending.
	reopened, err := tsrocksdb.NewStore(dbDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()

	version := int64(1)
	v1, err := reopened.GetAtVersion(testStoreKey, []byte("k1"), &version)
	require.NoError(t, err)
	require.Equal(t, []byte("v1"), v1)

	v2, err := reopened.GetAtVersion(testStoreKey, []byte("k2"), &version)
	require.NoError(t, err)
	require.Equal(t, []byte("v2"), v2)
}

// TestChangeSetToVersionDBCmdErrorReleasesStore verifies that a mid-migration
// failure both surfaces the underlying error and still releases the RocksDB
// handle/lock on the store it opened. It mirrors
// TestChangeSetToVersionDBCmdDurablyPersists by reopening a fresh Store
// against the same directory: RocksDB's exclusive file lock means that only
// succeeds if the command's own handle was actually closed on the error
// path, not just left open with the error silently eaten.
func TestChangeSetToVersionDBCmdErrorReleasesStore(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "versiondb")

	// Not a valid change set file: the header can't be parsed, so
	// IterateChangeSets/withChangeSetFile returns an error before the loop
	// completes.
	badFile := filepath.Join(dir, "bad.plain")
	require.NoError(t, os.WriteFile(badFile, []byte{0x01, 0x02, 0x03}, 0o600))

	cmd := ChangeSetToVersionDBCmd()
	cmd.SetArgs([]string{dbDir, badFile, "--" + flagStore, testStoreKey})
	require.Error(t, cmd.Execute())

	// Reopening the same directory only succeeds if the command closed its
	// own RocksDB handle on the error path; otherwise this fails with a
	// "lock hold by current process" IO error.
	reopened, err := tsrocksdb.NewStore(dbDir)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}
