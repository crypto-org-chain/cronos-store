package tsrocksdb

import (
	"errors"
	"fmt"

	"github.com/linxGnu/grocksdb"
)

// CompactVersionDB rewrites every sst file of the versiondb column family, bottommost level included,
// so all of them use the current compression, e.g. LZ4 or uncompressed files become ZSTD.
// The db must not be open in another process. rateBytesPerSec caps flush and compaction writes, 0 means unlimited.
func CompactVersionDB(dir string, rateBytesPerSec int64) (before, after uint64, err error) {
	if rateBytesPerSec < 0 {
		return 0, 0, fmt.Errorf("negative rate limit: %d", rateBytesPerSec)
	}
	db, cfHandle, err := openVersionDB(dir, compactionDBOpts(rateBytesPerSec))
	if err != nil {
		return 0, 0, fmt.Errorf("open versiondb: %w", err)
	}
	defer func() {
		cfHandle.Destroy()
		db.Close()
	}()

	before, _ = db.GetIntPropertyCF("rocksdb.live-sst-files-size", cfHandle)
	compactOpts := grocksdb.NewCompactRangeOptions()
	defer compactOpts.Destroy()
	// skips the bottommost files this same compaction just wrote, instead of rewriting them twice
	compactOpts.SetBottommostLevelCompaction(grocksdb.KForceOptimized)
	db.CompactRangeCFOpt(cfHandle, grocksdb.Range{}, compactOpts)

	// CompactRangeCFOpt returns no status, a failed compaction only shows up as a background error
	bgErrors, ok := db.GetIntProperty("rocksdb.background-errors")
	if !ok {
		return 0, 0, errors.New("failed to read rocksdb.background-errors")
	}
	if bgErrors > 0 {
		return 0, 0, fmt.Errorf("compaction failed with %d background errors, see the rocksdb LOG file", bgErrors)
	}
	after, _ = db.GetIntPropertyCF("rocksdb.live-sst-files-size", cfHandle)
	return before, after, nil
}
