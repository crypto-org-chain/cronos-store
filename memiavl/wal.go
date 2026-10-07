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
			return nil, fmt.Errorf("read wal dir fail: %w", listErr)
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
	Sync() error
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
	mu         sync.Mutex
	closed     bool
	segments   []*walSegment // ascending by first index
	cached     *walSegment   // most recently read non-tail segment
	firstIndex uint64
	lastIndex  uint64
}

type walSegment struct {
	index   uint64 // index of the first entry
	path    string
	entries [][]byte // loaded only for the tail and cached segments
}

func openReadOnlyWAL(dir string) (*readOnlyWAL, error) {
	var err error
	for range readOnlyWALOpenAttempts {
		var l *readOnlyWAL
		if l, err = tryOpenReadOnlyWAL(dir); !errors.Is(err, fs.ErrNotExist) {
			return l, err
		}
	}
	return nil, err
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
	data, err := readWALSegment(tail.path)
	if err != nil {
		return nil, err
	}
	// a torn last entry is one the writer hasn't finished appending.
	tail.entries, _ = parseWALEntries(data)
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

// readWALSegment falls back to the final name once the writer's TruncateFront renames a .START segment.
func readWALSegment(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && strings.HasSuffix(path, walStartSuffix) {
		return os.ReadFile(strings.TrimSuffix(path, walStartSuffix))
	}
	return data, err
}

// parseWALEntries splits binary-format segment data into entries; torn reports
// trailing bytes that don't form a complete entry.
func parseWALEntries(data []byte) (entries [][]byte, torn bool) {
	for len(data) > 0 {
		n, err := loadNextBinaryEntry(data)
		if err != nil {
			return entries, true
		}
		_, header := binary.Uvarint(data)
		entries = append(entries, data[header:n:n])
		data = data[n:]
	}
	return entries, false
}

func (l *readOnlyWAL) FirstIndex() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, wal.ErrClosed
	}
	if l.lastIndex == 0 {
		return 0, nil
	}
	return l.firstIndex, nil
}

func (l *readOnlyWAL) LastIndex() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, wal.ErrClosed
	}
	return l.lastIndex, nil
}

// Read returns a slice of the segment buffer, which must not be modified.
func (l *readOnlyWAL) Read(index uint64) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, wal.ErrClosed
	}
	if index == 0 || index < l.firstIndex || index > l.lastIndex {
		return nil, wal.ErrNotFound
	}

	i := sort.Search(len(l.segments), func(i int) bool { return l.segments[i].index > index }) - 1
	seg := l.segments[i]
	if seg.entries == nil {
		if err := l.loadSegment(seg); err != nil {
			return nil, err
		}
	}
	offset := index - seg.index
	if offset >= uint64(len(seg.entries)) {
		return nil, wal.ErrCorrupt
	}
	return seg.entries[offset], nil
}

// loadSegment loads a non-tail segment, evicting the previously cached one.
func (l *readOnlyWAL) loadSegment(seg *walSegment) error {
	data, err := readWALSegment(seg.path)
	if err != nil {
		return err
	}
	entries, torn := parseWALEntries(data)
	if torn {
		// only the tail can hold an entry the writer is still appending.
		return wal.ErrCorrupt
	}
	if l.cached != nil {
		l.cached.entries = nil
	}
	seg.entries = entries
	l.cached = seg
	return nil
}

func (*readOnlyWAL) WriteBatch(*wal.Batch) error { return errReadOnly }
func (*readOnlyWAL) TruncateFront(uint64) error  { return errReadOnly }
func (*readOnlyWAL) TruncateBack(uint64) error   { return errReadOnly }
func (*readOnlyWAL) Sync() error                 { return errReadOnly }

func (l *readOnlyWAL) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return wal.ErrClosed
	}
	l.closed = true
	l.segments, l.cached = nil, nil
	return nil
}
