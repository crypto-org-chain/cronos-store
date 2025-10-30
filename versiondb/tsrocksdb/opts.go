package tsrocksdb

import (
	"encoding/binary"
	"runtime"

	"github.com/linxGnu/grocksdb"
)

const VersionDBCFName = "versiondb"

type VersionDBConfig struct {
	CacheSizeMB        int
	BlockSizeKB        int
	CompressionThreads int
	Parallelism        int
}

// DefaultVersionDBConfig sets the Default versiondb config
func DefaultVersionDBConfig() *VersionDBConfig {
	return &VersionDBConfig{
		CacheSizeMB:        1024,
		BlockSizeKB:        32,
		CompressionThreads: 4,
		Parallelism:        runtime.NumCPU(),
	}
}

// NewVersionDBOpts returns the options used for the versiondb column family.
// FIXME: we don't enable dict compression for SSTFileWriter, because otherwise the file writer won't report correct file size.
// https://github.com/facebook/rocksdb/issues/11146
func NewVersionDBOpts(cfg *VersionDBConfig, sstFileWriter bool) *grocksdb.Options {
	if cfg == nil {
		cfg = DefaultVersionDBConfig()
	}
	opts := grocksdb.NewDefaultOptions()
	opts.SetCreateIfMissing(true)
	opts.SetComparator(CreateTSComparator())
	opts.IncreaseParallelism(cfg.Parallelism)
	opts.OptimizeLevelStyleCompaction(512 * 1024 * 1024)
	opts.SetTargetFileSizeMultiplier(2)
	opts.SetLevelCompactionDynamicLevelBytes(true)

	// block based table options
	bbto := grocksdb.NewDefaultBlockBasedTableOptions()

	// configurable via cfg.BlockSizeMB - in MB
	bbto.SetBlockSize(cfg.BlockSizeKB * 1024)
	bbto.SetBlockCache(grocksdb.NewLRUCache(uint64(cfg.CacheSizeMB) << 20))

	bbto.SetFilterPolicy(grocksdb.NewRibbonHybridFilterPolicy(9.9, 1))
	bbto.SetIndexType(grocksdb.KBinarySearchWithFirstKey)
	bbto.SetOptimizeFiltersForMemory(true)
	opts.SetBlockBasedTableFactory(bbto)
	// improve sst file creation speed: compaction or sst file writer.
	opts.SetCompressionOptionsParallelThreads(cfg.CompressionThreads)

	if !sstFileWriter {
		// compression options at bottommost level
		opts.SetBottommostCompression(grocksdb.ZSTDCompression)
		compressOpts := grocksdb.NewDefaultCompressionOptions()
		compressOpts.MaxDictBytes = 112640 // 110k
		compressOpts.Level = 12
		opts.SetBottommostCompressionOptions(compressOpts, true)
		opts.SetBottommostCompressionOptionsZstdMaxTrainBytes(compressOpts.MaxDictBytes*100, true)
	}
	return opts
}

// OpenVersionDBWithConfig opens versiondb with custom configuration options.
// The default column family is used for metadata;
// actual key-value pairs are stored on another column family named with "versiondb",
// which has user-defined timestamp enabled.
// This allows caller to customize configuration through VersionDBConfig.
func OpenVersionDBWithConfig(dir string, cfg *VersionDBConfig) (*grocksdb.DB, *grocksdb.ColumnFamilyHandle, error) {
	opts := grocksdb.NewDefaultOptions()
	opts.SetCreateIfMissing(true)
	opts.SetCreateIfMissingColumnFamilies(true)
	db, cfHandles, err := grocksdb.OpenDbColumnFamilies(
		opts, dir, []string{"default", VersionDBCFName},
		[]*grocksdb.Options{opts, NewVersionDBOpts(cfg, false)},
	)
	if err != nil {
		return nil, nil, err
	}
	return db, cfHandles[1], nil
}

// OpenVersionDB opens versiondb using default configuration.
func OpenVersionDB(dir string) (*grocksdb.DB, *grocksdb.ColumnFamilyHandle, error) {
	return OpenVersionDBWithConfig(dir, DefaultVersionDBConfig())
}

// OpenVersionDBForReadOnly open versiondb in readonly mode
func OpenVersionDBForReadOnly(dir string, cfg *VersionDBConfig, errorIfWalFileExists bool) (*grocksdb.DB, *grocksdb.ColumnFamilyHandle, error) {
	opts := grocksdb.NewDefaultOptions()
	db, cfHandles, err := grocksdb.OpenDbForReadOnlyColumnFamilies(
		opts, dir, []string{"default", VersionDBCFName},
		[]*grocksdb.Options{opts, NewVersionDBOpts(cfg, false)},
		errorIfWalFileExists,
	)
	if err != nil {
		return nil, nil, err
	}
	return db, cfHandles[1], nil
}

// OpenVersionDBAndTrimHistory opens versiondb similar to `OpenVersionDB`,
// but it also trim the versions newer than target one, can be used for rollback.
func OpenVersionDBAndTrimHistory(dir string, cfg *VersionDBConfig, version int64) (*grocksdb.DB, *grocksdb.ColumnFamilyHandle, error) {
	var ts [TimestampSize]byte
	binary.LittleEndian.PutUint64(ts[:], uint64(version))

	opts := grocksdb.NewDefaultOptions()
	opts.SetCreateIfMissing(true)
	opts.SetCreateIfMissingColumnFamilies(true)
	db, cfHandles, err := grocksdb.OpenDbAndTrimHistory(
		opts, dir, []string{"default", VersionDBCFName},
		[]*grocksdb.Options{opts, NewVersionDBOpts(cfg, false)},
		ts[:],
	)
	if err != nil {
		return nil, nil, err
	}
	return db, cfHandles[1], nil
}
