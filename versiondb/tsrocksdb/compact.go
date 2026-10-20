package tsrocksdb

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/linxGnu/grocksdb"
)

// CompactVersionDB rewrites every sst file of the versiondb column family, bottommost level included,
// with the current compression options. rateBytesPerSec caps flush and compaction writes, 0 means unlimited.
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

	before, err = liveSSTFilesSize(db, cfHandle)
	if err != nil {
		return 0, 0, err
	}
	compactOpts := grocksdb.NewCompactRangeOptions()
	defer compactOpts.Destroy()
	// skips the bottommost files this same compaction just wrote
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
	after, err = liveSSTFilesSize(db, cfHandle)
	if err != nil {
		return 0, 0, err
	}
	return before, after, nil
}

func liveSSTFilesSize(db *grocksdb.DB, cfHandle *grocksdb.ColumnFamilyHandle) (uint64, error) {
	size, ok := db.GetIntPropertyCF("rocksdb.live-sst-files-size", cfHandle)
	if !ok {
		return 0, errors.New("failed to read rocksdb.live-sst-files-size")
	}
	return size, nil
}

func compactionDBOpts(rateBytesPerSec int64) *grocksdb.Options {
	opts := grocksdb.NewDefaultOptions()
	// a manual compaction otherwise runs on a single thread
	opts.SetMaxSubcompactions(uint32(runtime.NumCPU()))
	if rateBytesPerSec > 0 {
		// refill period and fairness are rocksdb's defaults; SetRateLimiter takes ownership of the limiter
		opts.SetRateLimiter(grocksdb.NewGenericRateLimiter(rateBytesPerSec, 100_000, 10, grocksdb.RateLimiterModeWritesOnly, false))
	}
	return opts
}
