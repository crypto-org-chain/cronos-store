package tsrocksdb

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/linxGnu/grocksdb"
	"github.com/stretchr/testify/suite"

	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

const (
	compactTestKeys = 200
	// versions written by the ingested sst files, liveVersion is written through the db afterwards
	ingestedVersions = 3
	liveVersion      = ingestedVersions + 1
)

type valueFunc func(i, version int) []byte

type CompactSuite struct {
	suite.Suite
}

func TestCompactSuite(t *testing.T) {
	suite.Run(t, new(CompactSuite))
}

func compactTestKey(i int) []byte {
	return fmt.Appendf(nil, "k%04d", i)
}

func isDeleted(i, version int) bool {
	return i == 0 && version == ingestedVersions
}

// randomValue gives incompressible values, so compaction output size doesn't depend on the codec.
func randomValue(i, version int) []byte {
	if isDeleted(i, version) {
		return nil
	}
	value := make([]byte, 256)
	rand.New(rand.NewSource(int64(i*liveVersion + version))).Read(value)
	return value
}

func compressibleValue(i, version int) []byte {
	if isDeleted(i, version) {
		return nil
	}
	return fmt.Appendf(nil, `{"address":"0x%040x","balance":"%d","denom":"basecro","version":%d}`, i, i*1000+version, version)
}

// writeIngestedHistory mirrors build-versiondb-sst followed by ingest-versiondb-sst,
// which leaves LZ4 sst files in the bottommost level.
func (s *CompactSuite) writeIngestedHistory(dir string, value valueFunc) {
	// two files with disjoint key ranges, like build-versiondb-sst output split by file size
	var sstFiles []string
	for start := 0; start < compactTestKeys; start += compactTestKeys / 2 {
		sstPath := filepath.Join(s.T().TempDir(), fmt.Sprintf("history-%d.sst", start))
		w := grocksdb.NewSSTFileWriter(grocksdb.NewDefaultEnvOptions(), NewVersionDBOpts(true))
		s.Require().NoError(w.Open(sstPath))

		var ts [TimestampSize]byte
		for i := start; i < start+compactTestKeys/2; i++ {
			key := prependStoreKey(testStoreKey, compactTestKey(i))
			// sst writer requires the same order as the comparator: newer version first
			for version := ingestedVersions; version >= 1; version-- {
				binary.LittleEndian.PutUint64(ts[:], uint64(version))
				if v := value(i, version); v == nil {
					s.Require().NoError(w.DeleteWithTS(key, ts[:]))
				} else {
					s.Require().NoError(w.PutWithTS(key, ts[:], v))
				}
			}
		}
		s.Require().NoError(w.Finish())
		w.Destroy()
		sstFiles = append(sstFiles, sstPath)
	}

	db, cfHandle, err := OpenVersionDB(dir)
	s.Require().NoError(err)
	defer func() {
		cfHandle.Destroy()
		db.Close()
	}()
	ingestOpts := grocksdb.NewDefaultIngestExternalFileOptions()
	defer ingestOpts.Destroy()
	s.Require().NoError(db.IngestExternalFileCF(cfHandle, sstFiles, ingestOpts))
}

// writeLiveVersion leaves liveVersion in the wal, it's replayed into L0 when the db is reopened.
func (s *CompactSuite) writeLiveVersion(dir string, value valueFunc) {
	store, err := NewStore(dir)
	s.Require().NoError(err)
	defer store.Close()

	changeSet := make([]*types.StoreKVPair, 0, compactTestKeys)
	for i := 0; i < compactTestKeys; i++ {
		changeSet = append(changeSet, &types.StoreKVPair{StoreKey: testStoreKey, Key: compactTestKey(i), Value: value(i, liveVersion)})
	}
	s.Require().NoError(store.PutAtVersion(liveVersion, changeSet))
}

func (s *CompactSuite) liveSSTFiles(dir string) map[string]struct{} {
	db, cfHandle, err := OpenVersionDBForReadOnly(dir, false)
	s.Require().NoError(err)
	defer func() {
		cfHandle.Destroy()
		db.Close()
	}()

	files := make(map[string]struct{})
	for _, f := range db.GetLiveFilesMetaData() {
		if f.ColumnFamilyName == VersionDBCFName {
			files[f.Name] = struct{}{}
		}
	}
	return files
}

func (s *CompactSuite) requireHistory(dir string, value valueFunc, versions int) {
	store, err := NewStore(dir)
	s.Require().NoError(err)
	defer store.Close()

	for i := 0; i < compactTestKeys; i++ {
		for version := 1; version <= versions; version++ {
			v := int64(version)
			got, err := store.GetAtVersion(testStoreKey, compactTestKey(i), &v)
			s.Require().NoError(err)
			s.Require().Equal(value(i, version), got, "key %d version %d", i, version)
		}
	}
}

func (s *CompactSuite) TestCompactVersionDB() {
	testCases := []struct {
		name     string
		malleate func(dir string)
		// expected history, nil when the db has no data
		value    valueFunc
		versions int
		// upper bound of the after/before size ratio, 0 skips the check
		maxSizeRatio float64
		rateLimit    int64
		minDuration  time.Duration
		expErrMsg    string
	}{
		{
			name:      "missing db",
			malleate:  func(string) {},
			expErrMsg: "does not exist",
		},
		{
			name:      "negative rate limit",
			malleate:  func(dir string) { s.writeIngestedHistory(dir, randomValue) },
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
			// LZ4 to ZSTD with dictionary, a rewrite with unchanged compression stays close to 1
			name:         "ingested history is recompressed",
			malleate:     func(dir string) { s.writeIngestedHistory(dir, compressibleValue) },
			value:        compressibleValue,
			versions:     ingestedVersions,
			maxSizeRatio: 0.8,
		},
		{
			name: "ingested history with live writes in L0",
			malleate: func(dir string) {
				s.writeIngestedHistory(dir, randomValue)
				s.writeLiveVersion(dir, randomValue)
			},
			value:    randomValue,
			versions: liveVersion,
		},
		{
			// ~160KB of incompressible output at 64KiB/s needs well over a second; unthrottled it takes milliseconds
			name:        "ingested history with rate limit",
			malleate:    func(dir string) { s.writeIngestedHistory(dir, randomValue) },
			value:       randomValue,
			versions:    ingestedVersions,
			rateLimit:   64 << 10,
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

			if tc.value == nil {
				s.Require().Zero(before)
				s.Require().Zero(after)
				return
			}
			s.Require().Positive(after)
			if tc.maxSizeRatio > 0 {
				s.Require().Less(float64(after)/float64(before), tc.maxSizeRatio)
			}
			filesAfter := s.liveSSTFiles(dir)
			s.Require().NotEmpty(filesAfter)
			for name := range filesBefore {
				s.Require().NotContains(filesAfter, name, "sst file not rewritten")
			}
			s.requireHistory(dir, tc.value, tc.versions)
		})
	}
}
