package tsrocksdb

import (
	"bytes"
	"encoding/binary"

	"github.com/crypto-org-chain/cronos-store/versiondb"
	"github.com/linxGnu/grocksdb"
)

type rocksDBIterator struct {
	source             *grocksdb.Iterator
	prefix, start, end []byte
	isReverse          bool
	isInvalid          bool

	// see: https://github.com/crypto-org-chain/cronos-store/issues/1683
	skipVersionZero bool

	// readOpts must outlive the iterator because DBIter holds a pointer into the
	// rocksdb_readoptions_t struct (timestamp_ub_). Destroying ReadOptions while
	// the iterator is alive causes a dangling pointer and use-after-free.
	readOpts *grocksdb.ReadOptions
}

var _ versiondb.Iterator = (*rocksDBIterator)(nil)

func newRocksDBIterator(source *grocksdb.Iterator, prefix, start, end []byte, isReverse, skipVersionZero bool, readOpts *grocksdb.ReadOptions) *rocksDBIterator {
	if isReverse {
		if end == nil {
			source.SeekToLast()
		} else {
			source.Seek(end)
			if source.Valid() {
				eoakey := source.Key() // end or after key
				defer eoakey.Free()
				if bytes.Compare(end, eoakey.Data()) <= 0 {
					source.Prev()
				}
			} else {
				source.SeekToLast()
			}
		}
	} else {
		if start == nil {
			source.SeekToFirst()
		} else {
			source.Seek(start)
		}
	}
	it := &rocksDBIterator{
		source:          source,
		prefix:          prefix,
		start:           start,
		end:             end,
		isReverse:       isReverse,
		isInvalid:       false,
		skipVersionZero: skipVersionZero,
		readOpts:        readOpts,
	}

	it.trySkipZeroVersion()
	return it
}

// Domain implements Iterator.
func (itr *rocksDBIterator) Domain() ([]byte, []byte) {
	return itr.start, itr.end
}

// Valid implements Iterator.
func (itr *rocksDBIterator) Valid() bool {
	// Once invalid, forever invalid.
	if itr.isInvalid {
		return false
	}

	// If source has error, invalid.
	if err := itr.source.Err(); err != nil {
		itr.isInvalid = true
		return false
	}

	// If source is invalid, invalid.
	if !itr.source.Valid() {
		itr.isInvalid = true
		return false
	}

	// If key is end or past it, invalid.
	start := itr.start
	end := itr.end
	key := itr.source.Key()
	defer key.Free()
	if itr.isReverse {
		if start != nil && bytes.Compare(key.Data(), start) < 0 {
			itr.isInvalid = true
			return false
		}
	} else {
		if end != nil && bytes.Compare(end, key.Data()) <= 0 {
			itr.isInvalid = true
			return false
		}
	}

	// It's valid.
	return true
}

// Timestamp implements Iterator.
func (itr *rocksDBIterator) Timestamp() []byte {
	itr.assertIsValid()
	return moveSliceToBytes(itr.source.Timestamp())
}

// Key implements Iterator.
func (itr *rocksDBIterator) Key() []byte {
	itr.assertIsValid()
	return moveSliceToBytes(itr.source.Key())[len(itr.prefix):]
}

// Value implements Iterator.
func (itr *rocksDBIterator) Value() []byte {
	itr.assertIsValid()
	return moveSliceToBytes(itr.source.Value())
}

// Next implements Iterator.
func (itr *rocksDBIterator) Next() {
	itr.assertIsValid()
	if itr.isReverse {
		itr.source.Prev()
	} else {
		itr.source.Next()
	}

	itr.trySkipZeroVersion()
}

func (itr *rocksDBIterator) timestamp() uint64 {
	ts := itr.source.Timestamp()
	defer ts.Free()
	return binary.LittleEndian.Uint64(ts.Data())
}

func (itr *rocksDBIterator) trySkipZeroVersion() {
	if itr.skipVersionZero {
		for itr.Valid() && itr.timestamp() == 0 {
			if itr.isReverse {
				itr.source.Prev()
			} else {
				itr.source.Next()
			}
		}
	}
}

// Error implements Iterator.
func (itr *rocksDBIterator) Error() error {
	if itr.source == nil {
		return nil
	}
	return itr.source.Err()
}

// Close implements Iterator.
func (itr *rocksDBIterator) Close() error {
	var err error
	if itr.source != nil {
		err = itr.source.Err()
		itr.source.Close()
		itr.source = nil
	}
	if itr.readOpts != nil {
		itr.readOpts.Destroy()
		itr.readOpts = nil
	}
	return err
}
	itr.isInvalid = true
	var err error
	if itr.source != nil {
		err = itr.source.Err()
		itr.source.Close()
		itr.source = nil
	}
	if itr.readOpts != nil {
		itr.readOpts.Destroy()
		itr.readOpts = nil
	}
	return err
}

func (itr *rocksDBIterator) assertIsValid() {
	if !itr.Valid() {
		panic("iterator is invalid")
	}
}

// moveSliceToBytes will free the slice and copy out a go []byte
// This function can be applied on *Slice returned from Key() and Value()
// of an Iterator, because they are marked as freed.
func moveSliceToBytes(s *grocksdb.Slice) []byte {
	defer s.Free()
	if !s.Exists() {
		return nil
	}
	v := make([]byte, len(s.Data()))
	copy(v, s.Data())
	return v
}
