package tsrocksdb

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/linxGnu/grocksdb"
)

// rocksdb's own defaults for the generic rate limiter
const (
	rateLimiterRefillPeriodMicros = 100 * 1000
	rateLimiterFairness           = 10
)

// CompactVersionDB rewrites every sst file of the versiondb column family, bottommost level included,
// so files written with older options pick up the current compression, e.g. the LZ4 files produced by
// the sst file writer are recompressed with ZSTD. The db must not be open in another process.
// rateBytesPerSec caps flush and compaction writes, 0 means unlimited.
func CompactVersionDB(dir string, rateBytesPerSec int64) (before, after uint64, err error) {
	if rateBytesPerSec < 0 {
		return 0, 0, fmt.Errorf("negative rate limit: %d", rateBytesPerSec)
	}

	opts := grocksdb.NewDefaultOptions()
	defer opts.Destroy()
	// a single manual compaction runs on one thread unless split into subcompactions
	opts.SetMaxSubcompactions(uint32(runtime.NumCPU()))
	if rateBytesPerSec > 0 {
		opts.SetRateLimiter(grocksdb.NewGenericRateLimiter(
			rateBytesPerSec, rateLimiterRefillPeriodMicros, rateLimiterFairness,
			grocksdb.RateLimiterModeWritesOnly, false,
		))
	}
	cfOpts := NewVersionDBOpts(false)
	defer cfOpts.Destroy()

	db, cfHandles, err := grocksdb.OpenDbColumnFamilies(
		opts, dir, []string{defaultCFName, VersionDBCFName},
		[]*grocksdb.Options{opts, cfOpts},
	)
	if err != nil {
		return 0, 0, fmt.Errorf("open versiondb: %w", err)
	}
	defer func() {
		for _, handle := range cfHandles {
			handle.Destroy()
		}
		db.Close()
	}()
	cfHandle := cfHandles[1]

	before, _ = db.GetIntPropertyCF("rocksdb.live-sst-files-size", cfHandle)

	compactOpts := grocksdb.NewCompactRangeOptions()
	defer compactOpts.Destroy()
	// skips the bottommost files this same compaction just wrote, instead of rewriting them twice
	compactOpts.SetBottommostLevelCompaction(grocksdb.KForceOptimized)
	db.CompactRangeCFOpt(cfHandle, grocksdb.Range{}, compactOpts)

	// CompactRangeCFOpt returns no status, a failed compaction only shows up as a background error.
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
