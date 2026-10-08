package memiavl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/tidwall/gjson"
	"github.com/tidwall/wal"
)

// OpenWAL opens the write ahead log, try to truncate the corrupted tail if there's any
// TODO fix in upstream: https://github.com/tidwall/wal/pull/22
//
// It repairs files in place, so it must not be used on a WAL a live writer owns; see openReadOnlyWAL.
func OpenWAL(dir string, opts *wal.Options) (*wal.Log, error) {
	log, err := wal.Open(dir, opts)
	if errors.Is(err, wal.ErrCorrupt) {
		// try to truncate the corrupted tail: the last segment by wal.Open's naming rules,
		// so a stray file sorting after it is never truncated.
		segments, listErr := listWALSegments(dir)
		if listErr != nil {
			return nil, listErr
		}
		if len(segments) == 0 {
			return nil, err
		}
		if err = truncateCorruptedTail(segments[len(segments)-1].path, opts.LogFormat); err != nil {
			return nil, fmt.Errorf("truncate corrupted tail fail: %w", err)
		}

		// try again
		return wal.Open(dir, opts)
	}

	return log, err
}

func truncateCorruptedTail(path string, format wal.LogFormat) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	originalFileSize := len(data)
	var pos int
	for len(data) > 0 {
		var n int
		if format == wal.JSON {
			n, err = loadNextJSONEntry(data)
		} else {
			n, err = loadNextBinaryEntry(data)
		}
		if errors.Is(err, wal.ErrCorrupt) {
			break
		}
		if err != nil {
			return err
		}
		data = data[n:]
		pos += n
	}
	if pos < originalFileSize {
		return os.Truncate(path, int64(pos))
	}
	return nil
}

func loadNextJSONEntry(data []byte) (n int, err error) {
	// {"index":number,"data":string}
	idx := bytes.IndexByte(data, '\n')
	if idx == -1 {
		return 0, wal.ErrCorrupt
	}
	line := data[:idx]
	dres := gjson.Get(*(*string)(unsafe.Pointer(&line)), "data")
	if dres.Type != gjson.String {
		return 0, wal.ErrCorrupt
	}
	return idx + 1, nil
}

func loadNextBinaryEntry(data []byte) (n int, err error) {
	// data_size + data
	size, n := binary.Uvarint(data)
	if n <= 0 {
		return 0, wal.ErrCorrupt
	}
	if uint64(len(data)-n) < size {
		return 0, wal.ErrCorrupt
	}
	return n + int(size), nil
}

const (
	walStartSuffix = ".START"
	walEndSuffix   = ".END"
	// covers the writer's TruncateFront removing the tail segment between listing and reading it.
	readOnlyWALOpenAttempts = 3
)

// writeAheadLog is the WAL API used by DB, backed by *wal.Log when writable and
// by readOnlyWAL when read-only.
type writeAheadLog interface {
	FirstIndex() (uint64, error)
	LastIndex() (uint64, error)
	Read(index uint64) ([]byte, error)
	WriteBatch(b *wal.Batch) error
	TruncateFront(index uint64) error
	TruncateBack(index uint64) error
	Close() error
}

var (
	_ writeAheadLog = (*wal.Log)(nil)
	_ writeAheadLog = (*readOnlyWAL)(nil)
)

// readOnlyWAL reads a binary-format WAL that a live writer may be appending to
// or truncating. Unlike wal.Open it never creates, truncates, removes or renames
// files: a torn tail is an entry still being written, and .START/.END segments
// are a truncation in progress, so both are interpreted as wal.Open would
// recover them instead of being repaired.
type readOnlyWAL struct {
	// fixed at open
	segments   []*walSegment // ascending by first index
	firstIndex uint64
	lastIndex  uint64
	closed     atomic.Bool

	mu     sync.Mutex  // guards loading and evicting non-tail segment entries
	cached *walSegment // most recently read non-tail segment
}

type walSegment struct {
	index   uint64 // index of the first entry
	path    string
	entries [][]byte // loaded only for the tail and cached segments
}

func openReadOnlyWAL(dir string) (l *readOnlyWAL, err error) {
	for range readOnlyWALOpenAttempts {
		if l, err = tryOpenReadOnlyWAL(dir); !errors.Is(err, fs.ErrNotExist) {
			break
		}
	}
	return l, err
}

func tryOpenReadOnlyWAL(dir string) (*readOnlyWAL, error) {
	segments, err := listWALSegments(dir)
	if err != nil {
		return nil, err
	}
	l := &readOnlyWAL{segments: segments}
	if len(segments) == 0 {
		return l, nil
	}

	tail := segments[len(segments)-1]
	// a torn last entry is one the writer hasn't finished appending.
	if tail.entries, _, err = readWALEntries(tail.path); err != nil {
		return nil, err
	}
	l.firstIndex = segments[0].index
	// an empty tail (just cycled) ends the log at the previous segment; 0 means an empty log, as in wal.Log.
	l.lastIndex = tail.index + uint64(len(tail.entries)) - 1
	return l, nil
}

// listWALSegments returns the segments wal.Open would keep after recovering
// any .START/.END file, without touching the directory.
func listWALSegments(dir string) ([]*walSegment, error) {
	dirEntries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var (
		segments         []*walSegment
		hasStart, hasEnd bool
	)
	// os.ReadDir sorts by name: ascending index, and "N" before "N.END" before "N.START".
	for _, e := range dirEntries {
		name := e.Name()
		if e.IsDir() || len(name) < 20 {
			continue
		}
		index, err := strconv.ParseUint(name[:20], 10, 64)
		if err != nil || index == 0 {
			continue
		}
		seg := &walSegment{index: index, path: filepath.Join(dir, name)}
		switch name[20:] {
		case "":
			if !hasEnd {
				segments = append(segments, seg)
			}
		case walStartSuffix:
			// TruncateFront in progress: every earlier segment is being removed.
			hasStart = true
			segments = append(segments[:0], seg)
		case walEndSuffix:
			// TruncateBack interrupted: it replaces the segment it was cut from, and later ones are being removed.
			if hasEnd {
				continue
			}
			hasEnd = true
			if n := len(segments); n > 0 && segments[n-1].index == index {
				segments = segments[:n-1]
			}
			segments = append(segments, seg)
		}
	}
	if hasStart && hasEnd {
		return nil, wal.ErrCorrupt
	}
	return segments, nil
}

// readWALEntries splits a binary-format segment into entries; torn reports trailing
// bytes that don't form a complete entry. A .START segment the writer's TruncateFront
// has since renamed is read under its final name.
func readWALEntries(path string) (entries [][]byte, torn bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && strings.HasSuffix(path, walStartSuffix) {
		data, err = os.ReadFile(strings.TrimSuffix(path, walStartSuffix))
	}
	if err != nil {
		return nil, false, err
	}
	for len(data) > 0 {
		n, err := loadNextBinaryEntry(data)
		if err != nil {
			return entries, true, nil
		}
		_, header := binary.Uvarint(data)
		entries = append(entries, data[header:n:n])
		data = data[n:]
	}
	return entries, false, nil
}

func (l *readOnlyWAL) FirstIndex() (uint64, error) {
	if l.closed.Load() {
		return 0, wal.ErrClosed
	}
	if l.lastIndex == 0 {
		return 0, nil
	}
	return l.firstIndex, nil
}

func (l *readOnlyWAL) LastIndex() (uint64, error) {
	if l.closed.Load() {
		return 0, wal.ErrClosed
	}
	return l.lastIndex, nil
}

// Read returns a slice of the segment buffer, which must not be modified.
func (l *readOnlyWAL) Read(index uint64) ([]byte, error) {
	if l.closed.Load() {
		return nil, wal.ErrClosed
	}
	if index == 0 || index < l.firstIndex || index > l.lastIndex {
		return nil, wal.ErrNotFound
	}
	seg := l.segments[sort.Search(len(l.segments), func(i int) bool { return l.segments[i].index > index })-1]

	l.mu.Lock()
	defer l.mu.Unlock()
	if seg.entries == nil {
		entries, torn, err := readWALEntries(seg.path)
		if err != nil {
			return nil, err
		}
		if torn {
			// only the tail can hold an entry the writer is still appending.
			return nil, wal.ErrCorrupt
		}
		if l.cached != nil {
			l.cached.entries = nil
		}
		seg.entries, l.cached = entries, seg
	}
	offset := index - seg.index
	if offset >= uint64(len(seg.entries)) {
		return nil, wal.ErrCorrupt
	}
	return seg.entries[offset], nil
}

func (*readOnlyWAL) WriteBatch(*wal.Batch) error { return errReadOnly }
func (*readOnlyWAL) TruncateFront(uint64) error  { return errReadOnly }
func (*readOnlyWAL) TruncateBack(uint64) error   { return errReadOnly }

func (l *readOnlyWAL) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return wal.ErrClosed
	}
	return nil
}
