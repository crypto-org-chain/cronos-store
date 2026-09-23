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

	// Reopening forces a real RocksDB lock check, proving the command
	// actually closed its own handle.
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

func TestChangeSetToVersionDBCmdErrorReleasesStore(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "versiondb")

	badFile := filepath.Join(dir, "bad.plain")
	require.NoError(t, os.WriteFile(badFile, []byte{0x01, 0x02, 0x03}, 0o600))

	cmd := ChangeSetToVersionDBCmd()
	cmd.SetArgs([]string{dbDir, badFile, "--" + flagStore, testStoreKey})
	require.Error(t, cmd.Execute())

	// Reopening forces a real RocksDB lock check on the error path too.
	reopened, err := tsrocksdb.NewStore(dbDir)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}
