package client

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/cronos-store/versiondb/tsrocksdb"
	"github.com/stretchr/testify/suite"

	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

type CompactVersionDBCmdSuite struct {
	suite.Suite
}

func TestCompactVersionDBCmdSuite(t *testing.T) {
	suite.Run(t, new(CompactVersionDBCmdSuite))
}

func (s *CompactVersionDBCmdSuite) TestCompactVersionDBCmd() {
	testCases := []struct {
		name      string
		malleate  func(dir string)
		args      []string
		expErrMsg string
	}{
		{
			name:      "missing db",
			malleate:  func(string) {},
			expErrMsg: "does not exist",
		},
		{
			name: "rate limit overflows bytes per second",
			malleate: func(dir string) {
				store, err := tsrocksdb.NewStore(dir)
				s.Require().NoError(err)
				s.Require().NoError(store.Close())
			},
			args:      []string{"--" + flagRateLimit, fmt.Sprint(uint64(math.MaxInt64>>20) + 1)},
			expErrMsg: "rate limit too large",
		},
		{
			name: "compact with rate limit",
			malleate: func(dir string) {
				store, err := tsrocksdb.NewStore(dir)
				s.Require().NoError(err)
				s.Require().NoError(store.PutAtVersion(1, []*types.StoreKVPair{
					{StoreKey: testStoreKey, Key: []byte("k1"), Value: []byte("v1")},
				}))
				s.Require().NoError(store.Close())
			},
			args: []string{"--" + flagRateLimit, "1"},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			dir := filepath.Join(s.T().TempDir(), "versiondb")
			tc.malleate(dir)

			var out bytes.Buffer
			cmd := CompactVersionDBCmd()
			cmd.SetOut(&out)
			cmd.SetArgs(append([]string{dir}, tc.args...))
			err := cmd.Execute()
			if tc.expErrMsg != "" {
				s.Require().ErrorContains(err, tc.expErrMsg)
				return
			}
			s.Require().NoError(err)
			s.Require().Contains(out.String(), "sst files size:")

			// reopening proves the command released the rocksdb lock
			store, err := tsrocksdb.NewStore(dir)
			s.Require().NoError(err)
			defer store.Close()
			version := int64(1)
			value, err := store.GetAtVersion(testStoreKey, []byte("k1"), &version)
			s.Require().NoError(err)
			s.Require().Equal([]byte("v1"), value)
		})
	}
}
