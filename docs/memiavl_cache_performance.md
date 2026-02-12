# MemIAVL Cache Performance Benchmarks

Store height from 49031924, mainnet

## Test 1: Shared Cache (`song/shared_cache`)

```text
Cronos Archive Benchmark
Home Dir: /root/.snapshot-downloader/workspace/home/
Blocks Target: 100000
Blocks Synced: 100060
Duration: 7935s
Rate: 12.610 blocks/sec

Cache Metrics
Hits: 0
Misses: 0
Hit Rate: 0.00%

Storage Usage
VersionDB: 110.70 GB
MemIAVL: 75453.52 MB
Total Data: 3026.27 GB

Memory RSS: 43075.8 MB
Logs: ./archive_test_20260116_021813/node.log
```

## Test 2: No Cache (`song/rm_cache`)

```text
Cronos Archive Benchmark
Home Dir: /root/.snapshot-downloader/workspace/home/
Blocks Target: 100000
Blocks Synced: 100006
Duration: 7995s
Rate: 12.509 blocks/sec

Cache Metrics
Hits: 0
Misses: 0
Hit Rate: 0.00%

Storage Usage
VersionDB: 130.06 GB
MemIAVL: 75452.43 MB
Total Data: 3045.54 GB

Memory RSS: 43478.6 MB
Logs: ./archive_test_20260115_114401/node.log
```

## Test 3: Original LRU Cache (not thread-safe)

```text
Cronos Archive Benchmark
Home Dir: /root/.snapshot-downloader/workspace/home/
Blocks Target: 100000
Blocks Synced: 100021
Duration: 9640s
Rate: 10.376 blocks/sec

Cache Metrics
Hits: 0
Misses: 0
Hit Rate: 0.00%

Storage Usage
VersionDB: 109.10 GB
MemIAVL: 54624.72 MB
Total Data: 3005.15 GB

Memory RSS: 43037.6 MB
Logs: ./archive_test_20260120_040355/node.log
```

## Summary

| Test         | Rate (blocks/sec) | Duration | VersionDB | MemIAVL     | Memory RSS |
|--------------|-------------------|----------|-----------|-------------|------------|
| Shared Cache | 12.610            | 7935s    | 110.70 GB | 75453.52 MB | 43075.8 MB |
| No Cache     | 12.509            | 7995s    | 130.06 GB | 75452.43 MB | 43478.6 MB |
| Original LRU | 10.376            | 9640s    | 109.10 GB | 54624.72 MB | 43037.6 MB |
