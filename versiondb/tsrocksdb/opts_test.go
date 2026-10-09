package tsrocksdb

import (
	"fmt"
	"testing"

	"github.com/crypto-org-chain/cronos-store/versiondb"
	"github.com/stretchr/testify/require"
)

func TestFlushedFilesAreCompressed(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	defer store.Close()

	var raw int
	ch := make(chan versiondb.ImportEntry, 20_000)
	for i := 0; i < cap(ch); i++ {
		key := fmt.Appendf(nil, "k%08d", i)
		value := fmt.Appendf(nil, `{"address":"0x%040x","balance":"%d"}`, i, i*1000)
		raw += len(prependStoreKey(testStoreKey, key)) + TimestampSize + len(value)
		ch <- versiondb.ImportEntry{StoreKey: testStoreKey, Key: key, Value: value}
	}
	close(ch)
	require.NoError(t, store.Import(1, ch))
	require.NoError(t, store.Flush())

	size, _ := store.db.GetIntPropertyCF("rocksdb.live-sst-files-size", store.cfHandle)
	require.Less(t, float64(size)/float64(raw), 0.5, "flushed sst files are not compressed")
}
