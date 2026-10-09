package tsrocksdb

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/linxGnu/grocksdb"
	"github.com/stretchr/testify/suite"
)

const (
	historyKeys     = 200
	historyVersions = 4
)

type CompactSuite struct {
	suite.Suite
}

func TestCompactSuite(t *testing.T) {
	suite.Run(t, new(CompactSuite))
}

func historyKey(i int) []byte {
	return fmt.Appendf(nil, "k%04d", i)
}

// historyValue is nil for a key deleted at version 3, which is written again at version 4.
func historyValue(i, version int) []byte {
	if i == 0 && version == 3 {
		return nil
	}
	return fmt.Appendf(nil, `{"address":"0x%040x","balance":"%d","version":%d}`, i, i*1000+version, version)
}

// writeHistory ingests the history as an LZ4 sst file, like build-versiondb-sst followed by ingest-versiondb-sst.
func (s *CompactSuite) writeHistory(dir string) {
	sstPath := filepath.Join(s.T().TempDir(), "history.sst")
	w := grocksdb.NewSSTFileWriter(grocksdb.NewDefaultEnvOptions(), NewVersionDBOpts(true))
	defer w.Destroy()
	s.Require().NoError(w.Open(sstPath))
	var ts [TimestampSize]byte
	for i := 0; i < historyKeys; i++ {
		key := prependStoreKey(testStoreKey, historyKey(i))
		// the sst writer needs the comparator order: newer version first
		for version := historyVersions; version >= 1; version-- {
			binary.LittleEndian.PutUint64(ts[:], uint64(version))
			if value := historyValue(i, version); value == nil {
				s.Require().NoError(w.DeleteWithTS(key, ts[:]))
			} else {
				s.Require().NoError(w.PutWithTS(key, ts[:], value))
			}
		}
	}
	s.Require().NoError(w.Finish())

	store, err := NewStore(dir)
	s.Require().NoError(err)
	defer store.Close()
	s.Require().NoError(store.db.IngestExternalFileCF(store.cfHandle, []string{sstPath}, grocksdb.NewDefaultIngestExternalFileOptions()))
}

func (s *CompactSuite) liveSSTFiles(dir string) map[string]struct{} {
	db, cfHandle, err := OpenVersionDBForReadOnly(dir, false)
	s.Require().NoError(err)
	defer NewStoreWithDB(db, cfHandle).Close()

	files := make(map[string]struct{})
	for _, f := range db.GetLiveFilesMetaData() {
		if f.ColumnFamilyName == VersionDBCFName {
			files[f.Name] = struct{}{}
		}
	}
	return files
}

func (s *CompactSuite) requireHistory(dir string) {
	store, err := NewStore(dir)
	s.Require().NoError(err)
	defer store.Close()

	for i := 0; i < historyKeys; i++ {
		for version := 1; version <= historyVersions; version++ {
			v := int64(version)
			value, err := store.GetAtVersion(testStoreKey, historyKey(i), &v)
			s.Require().NoError(err)
			s.Require().Equal(historyValue(i, version), value, "key %d version %d", i, version)
		}
	}
}

func (s *CompactSuite) TestCompactVersionDB() {
	testCases := []struct {
		name        string
		malleate    func(dir string)
		rateLimit   int64
		minDuration time.Duration
		expErrMsg   string
	}{
		{
			name:      "missing db",
			malleate:  func(string) {},
			expErrMsg: "does not exist",
		},
		{
			name:      "negative rate limit",
			malleate:  s.writeHistory,
			rateLimit: -1,
			expErrMsg: "negative rate limit",
		},
		{
			name: "db held by another handle",
			malleate: func(dir string) {
				store, err := NewStore(dir)
				s.Require().NoError(err)
				s.T().Cleanup(func() { store.Close() })
			},
			expErrMsg: "lock",
		},
		{
			name: "empty db",
			malleate: func(dir string) {
				store, err := NewStore(dir)
				s.Require().NoError(err)
				s.Require().NoError(store.Close())
			},
		},
		{
			name:     "history",
			malleate: s.writeHistory,
		},
		{
			// the compaction writes ~4.5 KB, ~2 s at 2 KiB/s; unthrottled it takes milliseconds
			name:        "history with rate limit",
			malleate:    s.writeHistory,
			rateLimit:   2 << 10,
			minDuration: time.Second,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			dir := filepath.Join(s.T().TempDir(), "versiondb")
			tc.malleate(dir)

			var filesBefore map[string]struct{}
			if tc.expErrMsg == "" {
				filesBefore = s.liveSSTFiles(dir)
			}

			start := time.Now()
			before, after, err := CompactVersionDB(dir, tc.rateLimit)
			if tc.expErrMsg != "" {
				s.Require().ErrorContains(err, tc.expErrMsg)
				return
			}
			s.Require().NoError(err)
			s.Require().GreaterOrEqual(time.Since(start), tc.minDuration)
			if len(filesBefore) == 0 {
				s.Require().Zero(after)
				return
			}

			// LZ4 and uncompressed files become ZSTD with a dictionary; a rewrite with unchanged compression stays close to 1
			s.Require().Less(float64(after)/float64(before), 0.8)
			filesAfter := s.liveSSTFiles(dir)
			for name := range filesBefore {
				s.Require().NotContains(filesAfter, name, "sst file not rewritten")
			}
			s.requireHistory(dir)
		})
	}
}
