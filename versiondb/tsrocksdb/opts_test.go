package tsrocksdb

import (
	"fmt"
	"testing"

	"github.com/crypto-org-chain/cronos-store/versiondb"
	"github.com/stretchr/testify/suite"

	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

const flushTestPairs = 20_000

type FlushCompressionSuite struct {
	suite.Suite
}

func TestFlushCompressionSuite(t *testing.T) {
	suite.Run(t, new(FlushCompressionSuite))
}

func flushTestKey(i int) []byte {
	return fmt.Appendf(nil, "k%08d", i)
}

func flushTestValue(i int) []byte {
	return fmt.Appendf(nil, `{"address":"0x%040x","balance":"%d","denom":"basecro"}`, i, i*1000)
}

// Flushed files must be compressed: sorted bulk writes don't overlap, so compaction moves them down
// to the bottommost level as they are, without a rewrite.
func (s *FlushCompressionSuite) TestFlushedFilesAreCompressed() {
	testCases := []struct {
		name  string
		write func(store Store)
	}{
		{
			name: "import",
			write: func(store Store) {
				ch := make(chan versiondb.ImportEntry)
				go func() {
					for i := 0; i < flushTestPairs; i++ {
						ch <- versiondb.ImportEntry{StoreKey: testStoreKey, Key: flushTestKey(i), Value: flushTestValue(i)}
					}
					close(ch)
				}()
				s.Require().NoError(store.Import(1, ch))
			},
		},
		{
			name: "put at version",
			write: func(store Store) {
				changeSet := make([]*types.StoreKVPair, 0, flushTestPairs)
				for i := 0; i < flushTestPairs; i++ {
					changeSet = append(changeSet, &types.StoreKVPair{StoreKey: testStoreKey, Key: flushTestKey(i), Value: flushTestValue(i)})
				}
				s.Require().NoError(store.PutAtVersion(1, changeSet))
			},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			store, err := NewStore(s.T().TempDir())
			s.Require().NoError(err)
			defer store.Close()

			tc.write(store)
			s.Require().NoError(store.Flush())

			var raw int
			for i := 0; i < flushTestPairs; i++ {
				raw += len(prependStoreKey(testStoreKey, flushTestKey(i))) + TimestampSize + len(flushTestValue(i))
			}
			size, ok := store.db.GetIntPropertyCF("rocksdb.live-sst-files-size", store.cfHandle)
			s.Require().True(ok)
			s.Require().Positive(size)
			s.Require().Less(float64(size)/float64(raw), 0.5, "flushed sst files are not compressed")
		})
	}
}
