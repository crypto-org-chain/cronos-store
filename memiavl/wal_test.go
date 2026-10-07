package memiavl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/tidwall/wal"
)

func TestCorruptedTail(t *testing.T) {
	opts := &wal.Options{
		LogFormat: wal.JSON,
	}
	dir := t.TempDir()

	testCases := []struct {
		name      string
		logs      []byte
		lastIndex uint64
	}{
		{"failure-1", []byte("\n"), 0},
		{"failure-2", []byte(`{}` + "\n"), 0},
		{"failure-3", []byte(`{"index":"1"}` + "\n"), 0},
		{"failure-4", []byte(`{"index":"1","data":"?"}`), 0},
		{"failure-5", []byte(`{"index":1,"data":"?"}` + "\n" + `{"index":"1","data":"?"}`), 1},
		// entry is 23 bytes (including newline); tail is also 23 bytes to exercise pos == len(tail) parity.
		{"failure-6-equal-length-tail", []byte(`{"index":1,"data":"?"}` + "\n" + strings.Repeat("?", 23)), 1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := os.WriteFile(filepath.Join(dir, "00000000000000000001"), tc.logs, 0o600)
			require.NoError(t, err)

			_, err = wal.Open(dir, opts)
			require.Equal(t, wal.ErrCorrupt, err)

			log, err := OpenWAL(dir, opts)
			require.NoError(t, err)

			lastIndex, err := log.LastIndex()
			require.NoError(t, err)
			require.Equal(t, tc.lastIndex, lastIndex)
		})
	}
}

// 10-byte entries with a 50-byte segment size cycle a segment every 5 entries,
// so segments start at 1, 6, 11 and 16 (the tail, holding 16-18).
const roWALEntries = 18

type ReadOnlyWALTestSuite struct {
	suite.Suite
	dir string
}

func TestReadOnlyWALTestSuite(t *testing.T) {
	suite.Run(t, new(ReadOnlyWALTestSuite))
}

func (s *ReadOnlyWALTestSuite) SetupTest() {
	s.dir = s.T().TempDir()
	writer, err := wal.Open(s.dir, &wal.Options{SegmentSize: 50, NoSync: true})
	s.Require().NoError(err)
	s.T().Cleanup(func() { s.Require().NoError(writer.Close()) })
	for i := uint64(1); i <= roWALEntries; i++ {
		s.Require().NoError(writer.Write(i, roWALEntry(i)))
	}
}

func roWALEntry(index uint64) []byte {
	return []byte(fmt.Sprintf("entry-%03d", index))
}

func (s *ReadOnlyWALTestSuite) segmentPath(index uint64, suffix string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%020d", index)+suffix)
}

// writeSegment writes entries [from, to] as the writer's TruncateFront/TruncateBack would.
func (s *ReadOnlyWALTestSuite) writeSegment(name string, from, to uint64) {
	var buf []byte
	for i := from; i <= to; i++ {
		buf = binary.AppendUvarint(buf, uint64(len(roWALEntry(i))))
		buf = append(buf, roWALEntry(i)...)
	}
	s.Require().NoError(os.WriteFile(name, buf, 0o600))
}

func (s *ReadOnlyWALTestSuite) removeSegments(indexes ...uint64) {
	for _, index := range indexes {
		s.Require().NoError(os.Remove(s.segmentPath(index, "")))
	}
}

// snapshotDir returns nil when dir doesn't exist, so a created directory shows up as a change.
func (s *ReadOnlyWALTestSuite) snapshotDir() map[string][]byte {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	s.Require().NoError(err)
	files := make(map[string][]byte, len(entries))
	for _, e := range entries {
		path := filepath.Join(s.dir, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			s.Require().NoError(err)
			files[e.Name()] = []byte("-> " + target)
			continue
		}
		files[e.Name()], err = os.ReadFile(path)
		s.Require().NoError(err)
	}
	return files
}

func (s *ReadOnlyWALTestSuite) TestOpenReadOnlyWAL() {
	readErrIn := func(from, to uint64, err error) func(uint64) error {
		return func(index uint64) error {
			if index >= from && index <= to {
				return err
			}
			return nil
		}
	}

	testCases := []struct {
		name       string
		malleate   func()
		afterOpen  func()
		expFirst   uint64
		expLast    uint64
		expErr     error
		expReadErr func(index uint64) error
	}{
		{
			name:     "missing dir",
			malleate: func() { s.dir = filepath.Join(s.dir, "missing") },
		},
		{
			name:     "live log",
			malleate: func() {},
			expFirst: 1,
			expLast:  roWALEntries,
		},
		{
			name: "torn tail",
			malleate: func() {
				f, err := os.OpenFile(s.segmentPath(16, ""), os.O_APPEND|os.O_WRONLY, 0)
				s.Require().NoError(err)
				_, err = f.Write(append(binary.AppendUvarint(nil, 9), "ent"...))
				s.Require().NoError(err)
				s.Require().NoError(f.Close())
			},
			expFirst: 1,
			expLast:  roWALEntries,
		},
		{
			name:     "truncate front writing temp file",
			malleate: func() { s.Require().NoError(os.WriteFile(filepath.Join(s.dir, "TEMP"), []byte("partial"), 0o600)) },
			expFirst: 1,
			expLast:  roWALEntries,
		},
		{
			name:     "truncate front started",
			malleate: func() { s.writeSegment(s.segmentPath(13, walStartSuffix), 13, 15) },
			expFirst: 13,
			expLast:  roWALEntries,
		},
		{
			name: "truncate front removed old segments",
			malleate: func() {
				s.writeSegment(s.segmentPath(13, walStartSuffix), 13, 15)
				s.removeSegments(1, 6, 11)
			},
			expFirst: 13,
			expLast:  roWALEntries,
		},
		{
			name:     "truncate front finished after open",
			malleate: func() { s.writeSegment(s.segmentPath(13, walStartSuffix), 13, 15) },
			afterOpen: func() {
				s.removeSegments(1, 6, 11)
				s.Require().NoError(os.Rename(s.segmentPath(13, walStartSuffix), s.segmentPath(13, "")))
			},
			expFirst: 13,
			expLast:  roWALEntries,
		},
		{
			name:     "truncate front of tail",
			malleate: func() { s.writeSegment(s.segmentPath(17, walStartSuffix), 17, 18) },
			expFirst: 17,
			expLast:  roWALEntries,
		},
		{
			name:     "truncate back interrupted",
			malleate: func() { s.writeSegment(s.segmentPath(11, walEndSuffix), 11, 12) },
			expFirst: 1,
			expLast:  12,
		},
		{
			name: "second truncate back ignored",
			malleate: func() {
				s.writeSegment(s.segmentPath(11, walEndSuffix), 11, 12)
				s.writeSegment(s.segmentPath(16, walEndSuffix), 16, 17)
			},
			expFirst: 1,
			expLast:  12,
		},
		{
			name: "tail removed on every attempt",
			malleate: func() {
				s.Require().NoError(os.Symlink(filepath.Join(s.dir, "gone"), s.segmentPath(99, "")))
			},
			expErr: fs.ErrNotExist,
		},
		{
			name:       "older segment removed after open",
			malleate:   func() {},
			afterOpen:  func() { s.removeSegments(6) },
			expFirst:   1,
			expLast:    roWALEntries,
			expReadErr: readErrIn(6, 10, fs.ErrNotExist),
		},
		{
			// only the tail can hold an entry still being written.
			name:       "older segment torn",
			malleate:   func() { s.Require().NoError(os.Truncate(s.segmentPath(6, ""), 47)) },
			expFirst:   1,
			expLast:    roWALEntries,
			expReadErr: readErrIn(6, 10, wal.ErrCorrupt),
		},
		{
			name:       "older segment short",
			malleate:   func() { s.Require().NoError(os.Truncate(s.segmentPath(6, ""), 30)) },
			expFirst:   1,
			expLast:    roWALEntries,
			expReadErr: readErrIn(9, 10, wal.ErrCorrupt),
		},
		{
			name: "truncate front and back",
			malleate: func() {
				s.writeSegment(s.segmentPath(13, walStartSuffix), 13, 15)
				s.writeSegment(s.segmentPath(16, walEndSuffix), 16, 17)
			},
			expErr: wal.ErrCorrupt,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()
			tc.malleate()
			before := s.snapshotDir()

			l, err := openReadOnlyWAL(s.dir)
			if tc.expErr != nil {
				s.Require().ErrorIs(err, tc.expErr)
				s.Require().Equal(before, s.snapshotDir())
				return
			}
			s.Require().NoError(err)
			if tc.afterOpen != nil {
				tc.afterOpen()
				before = s.snapshotDir()
			}

			first, err := l.FirstIndex()
			s.Require().NoError(err)
			s.Require().Equal(tc.expFirst, first)
			last, err := l.LastIndex()
			s.Require().NoError(err)
			s.Require().Equal(tc.expLast, last)
			for i := tc.expFirst; i != 0 && i <= tc.expLast; i++ {
				data, err := l.Read(i)
				if tc.expReadErr != nil && tc.expReadErr(i) != nil {
					s.Require().ErrorIs(err, tc.expReadErr(i), "index %d", i)
					continue
				}
				s.Require().NoError(err, "index %d", i)
				s.Require().Equal(roWALEntry(i), data, "index %d", i)
			}
			_, err = l.Read(tc.expLast + 1)
			s.Require().ErrorIs(err, wal.ErrNotFound)

			s.Require().NoError(l.Close())
			s.Require().Equal(before, s.snapshotDir())
		})
	}
}
