# cronos-store Performance Issues

> Generated from deep performance analysis (staff-engineer-perf-architect + per-issue impact analysis).
> Last updated: 2026-03-16

---

## Summary

| # | Issue | File | Priority | Effort | Impact | Status |
|---|-------|------|----------|--------|--------|--------|
| 1 | [sha256 pool in HashNode](#1-sha256-pool-in-hashnode) | `memiavl/node.go:191` | HIGH | Low | ~1–20 MB GC/block | ✅ Fixed — PR #48 |
| 2 | [Historical DB cache](#2-historical-db-cache) | `store/rootmulti/store.go:205` | HIGH | Medium | 2–58 ms/query, fd leak | 🔄 In Progress |
| 3 | [storePrefix fmt.Sprintf](#3-storeprefix-fmtsprintf) | `versiondb/tsrocksdb/store.go:356` | MEDIUM | Trivial | ~400–800 µs/block | ⬜ Open |
| 4 | [Telemetry time.Now() overhead](#4-telemetry-timenow-overhead) | `versiondb/store.go:40` | MEDIUM | Trivial | ~250–500 µs/block | ⬜ Open |
| 5 | [versiondb iterator byte copies](#5-versiondb-iterator-byte-copies) | `versiondb/tsrocksdb/iterator.go:106` | MEDIUM | Medium | ~4 ms per 10K scan | ⬜ Open |
| 6 | [RocksDB ReadOptions per read](#6-rocksdb-readoptions-per-read) | `versiondb/tsrocksdb/store.go:339` | MEDIUM | Low | ~100–200 ns/read | ⬜ Open |
| 7 | [MemNode allocation pressure](#7-memnode-allocation-pressure) | `memiavl/node.go`, `mem_node.go` | MEDIUM | High | ~3.5 MB/block | ⬜ Open |
| 8 | [sharedCache write lock on Get](#8-sharedcache-write-lock-on-get) | `memiavl/tree.go:81` | LOW | N/A | Correct as-is | ✅ No fix needed |
| 9 | [Spin-loop time.Sleep](#9-spin-loop-timesleep) | `memiavl/db.go:514` | LOW | Low | <5 µs amortized | ⬜ Open |
| 10 | [snapshotName fmt.Sprintf](#10-snapshotname-fmtsprintf) | `memiavl/db.go:1064` | LOW | Trivial | Once/commit | ⬜ Open |
| 11 | [bytes.Compare pattern](#11-bytescompare-pattern) | `memiavl/mem_node.go:163` | LOW | Trivial | Micro | ⬜ Open |
| 12 | [Iterator stack not pre-allocated](#12-iterator-stack-not-pre-allocated) | `memiavl/iterator.go:19` | LOW | Trivial | Minor | ⬜ Open |
| 13 | [MultiTree.Copy map rebuild](#13-multitreecopy-map-rebuild) | `memiavl/multitree.go:170` | LOW | Trivial | Once/1000 blocks | ⬜ Open |
| 14 | [Exporter channel-based allocs](#14-exporter-channel-based-allocs) | `memiavl/export.go:117` | LOW | Medium | Snapshot export only | ⬜ Open |
| 15 | [WAL marshal allocation](#15-wal-marshal-allocation) | `memiavl/db.go:1290` | LOW | Low | 1 alloc/block | ⬜ Open |
| 16 | [flush() unoptimised sort](#16-flush-unoptimised-sort) | `store/rootmulti/store.go:73` | LOW | Trivial | 30 allocs/block | ⬜ Open |
| 17 | [Proof generation allocs](#17-proof-generation-allocs) | `memiavl/proof.go:111` | LOW | Low | Query path only | ⬜ Open |

---

## HIGH Priority

### 1. sha256 pool in HashNode

**File:** `memiavl/node.go:191`
**Status:** ✅ **Fixed — [PR #48](https://github.com/crypto-org-chain/cronos-store/pull/48)**

#### Problem
`HashNode()` called `sha256.New()` on every dirty `MemNode` during `SaveVersion(true)`, allocating a new ~96-byte `sha256.digest` per call. With thousands of KV writes per block this generates thousands of short-lived heap allocations.

#### Impact (measured)
| Scenario | sha256.New() calls | Garbage created |
|----------|-------------------|-----------------|
| Normal block (500 txs, ~10K KV writes) | ~9,000 | ~860 KB/block |
| Heavy block (2K txs, ~100K KV writes) | ~100K–200K | ~10–20 MB/block |

`PersistedNode.Hash()` is unaffected — it reads directly from the mmap'd snapshot buffer.

#### Fix
Added `sync.Pool` for `hash.Hash` objects. `Get` → `Reset` → compute → `Put` back.

#### Benchmarks (Apple M1 Max)
```
BenchmarkHashNodeLeaf      221 ns/op   112 B/op   5 allocs/op  ← pooled (new)
BenchmarkHashNodeNoPool    261 ns/op   240 B/op   6 allocs/op  ← sha256.New() (old)
```
**−15% ns/op · −53% bytes/op · −1 alloc/op**

---

### 2. Historical DB cache

**File:** `store/rootmulti/store.go:205–232` (also `:543–559`)
**Status:** 🔄 **In Progress**

#### Problem
Every historical ABCI query (any version ≠ current) calls `memiavl.Load()` which costs:
- ~270 syscalls: 90× `open()` + 90× `mmap()` + 90× `fstat()` + directory scans (for 30 stores)
- WAL replay: up to 999 blocks of changeset replay from nearest snapshot

There is zero caching — repeated queries to the same height pay the full cost every time.

**Additional bug:** `CacheMultiStoreWithVersion()` leaks the opened `*memiavl.DB` — it never calls `db.Close()`. At 30 stores × 3 files = 90 fds per open DB, just 11 concurrent un-GC'd queries can exhaust the default fd limit (1024).

#### Impact
| Scenario | Latency per query |
|----------|------------------|
| Snapshot boundary, warm FS | ~1.5 ms |
| Mid-interval (avg), warm FS | ~18 ms |
| Mid-interval (avg), cold FS | ~21 ms |
| Worst case (K=999 WAL replay) | ~58 ms |
| Current-version query (baseline) | <0.1 ms |

**Overhead ratio: 15×–580× slower than current-version queries.**

Primary victims: IBC relayer nodes (proof queries at specific heights), public API nodes (gRPC historical queries), EVM RPC nodes (`eth_call` at historical blocks).

#### Fix
Add a small LRU cache (4 entries) of `*memiavl.DB` instances with reference counting:
- `Query()` path: borrow from cache → use → release (bounded lifetime, clean)
- `CacheMultiStoreWithVersion()` path: borrow from cache → release via finalizer (fixes fd leak)
- `Store.Close()`: drain and close all cached DB instances

---

## MEDIUM Priority

### 3. storePrefix fmt.Sprintf

**File:** `versiondb/tsrocksdb/store.go:356`
**Status:** ⬜ Open

#### Problem
```go
func storePrefix(storeKey string) []byte {
    return []byte(fmt.Sprintf(StorePrefixTpl, storeKey))  // "s/k:%s/"
}
```
Called on every `GetAtVersion`, `HasAtVersion`, and iterator creation. `fmt.Sprintf` parses the format string, allocates an internal buffer, and produces 2 allocations.

#### Impact
| Metric | fmt.Sprintf | Direct concat | Savings |
|--------|-------------|---------------|---------|
| Latency | ~80–120 ns | ~20–30 ns | ~60–90 ns/call |
| Allocations | 2 | 1 | 1/call |
| Per block (5K–20K reads) | — | — | **~400–800 µs** |

#### Fix
```go
func storePrefix(storeKey string) []byte {
    return []byte("s/k:" + storeKey + "/")
}
```
One-line change, zero risk.

---

### 4. Telemetry time.Now() overhead

**File:** `versiondb/store.go:40–56`
**Status:** ⬜ Open

#### Problem
```go
func (st *Store) Get(key []byte) []byte {
    defer telemetry.MeasureSince(time.Now(), "store", "versiondb", "get")
    ...
}
```
`time.Now()` is evaluated eagerly (as a `defer` argument), calling a VDSO syscall (~20 ns on Linux) on every Get/Has — even when telemetry is disabled, which is the case for the majority of validators. `MeasureSince()` immediately returns when telemetry is disabled, making the `time.Now()` call pure waste.

#### Impact
| Metric | Value |
|--------|-------|
| time.Now() cost (Linux VDSO) | ~15–25 ns |
| Per block (10K Get/Has calls) | **~250–500 µs** |

#### Fix
Replace `time.Now()` with `telemetry.Now()` (already exists in the SDK):
```go
defer telemetry.MeasureSince(telemetry.Now(), "store", "versiondb", "get")
```
`telemetry.Now()` guards the syscall behind `IsTelemetryEnabled()`, reducing the disabled path to ~3 ns.

**Combined with Issue #3:** both fixes together save **~650 µs–1.3 ms per block** in the versiondb query path.

---

### 5. versiondb iterator byte copies

**File:** `versiondb/tsrocksdb/iterator.go:106–121`
**Status:** ⬜ Open

#### Problem
Every call to `Key()`, `Value()`, or `Timestamp()` calls `moveSliceToBytes()` which allocates a new `[]byte` and copies from RocksDB-managed memory. Worse, `assertIsValid()` (called inside `Key()` and `Value()`) calls `Valid()` which calls `source.Key()` again — so a single `iter.Key()` call fetches the key from RocksDB **3 times** via CGO.

**CGO crossings per iterator step** (Valid + Key + Value + Next):
| Operation | CGO crossings |
|-----------|--------------|
| `Valid()` check | 2 |
| `Key()` → assertIsValid → Valid | 2 |
| `Key()` → source.Key() | 2 |
| `Value()` → assertIsValid → Valid | 2 |
| `Value()` → source.Value() | 2 |
| **Total per step** | **~12–14** |

#### Impact
| Workload | CGO overhead | Allocations |
|----------|-------------|-------------|
| Paginated query (100 items) | ~42 µs | ~500 B |
| Full store scan (10K items) | **~4.2 ms** | ~160 KB garbage |

#### Fix
Cache the current key/value in the iterator struct; only re-fetch on `Next()`/`Prev()`. This reduces CGO crossings per step from 12–14 down to 3–4.

---

### 6. RocksDB ReadOptions per read

**File:** `versiondb/tsrocksdb/store.go:339–353`
**Status:** ⬜ Open

#### Problem
```go
func (s Store) GetAtVersionSlice(...) (*grocksdb.Slice, error) {
    readOpts := newTSReadOptions(version)   // CGO malloc
    defer readOpts.Destroy()               // CGO free
    ...
}
```
Every single `Get`/`Has` call allocates a new `ReadOptions` object via CGO and destroys it after. This is 4 CGO crossings (create, SetTimestamp, read, destroy) for what should be 1.

#### Impact
| Metric | Value |
|--------|-------|
| CGO overhead per Get | ~100–200 ns (3 extra crossings) |
| At 100 QPS × 50 unique reads | ~500 µs/s wasted |

#### Fix
Cache the `ReadOptions` per version (or per Store), built once and reused for all reads at the same version. Since the version is fixed at `CacheMultiStoreWithVersion` time, a single `ReadOptions` could serve all reads in the query context.

---

### 7. MemNode allocation pressure

**Files:** `memiavl/node.go`, `memiavl/mem_node.go`
**Status:** ⬜ Open

#### Problem
Every KV write to the IAVL tree clones all ancestor nodes from the leaf up to the root via `Mutate()` (copy-on-write). MemNode is ~120 bytes. For a tree of depth 30:
- 1 update = ~30 MemNode allocations (~3.6 KB)
- 1 insert = ~32 MemNode allocations (~3.8 KB)

**Per block (1,000 KV changes across all stores):**
| Metric | Value |
|--------|-------|
| MemNode allocations | ~30,600 |
| Bytes allocated | ~3.5 MB |
| GC pause impact | <1 ms/GC cycle (current load) |

MemNodes accumulate for up to 1,000 blocks (the snapshot interval) before being freed.

#### Fix
Consider `sync.Pool` for `MemNode` structs. After snapshot rewrite, pooled nodes can be reclaimed. The COW design is fundamental to correctness — any change must be careful.

**Note:** This is inherent to the COW-AVL design. Only worth tackling if block times drop below 500ms or KV change volume grows 10x.

---

## LOW Priority

### 8. sharedCache write lock on Get

**File:** `memiavl/tree.go:81–89`
**Status:** ✅ **No fix needed — correct as-is**

The write lock (`Lock()`) on `sharedCache.Get()` is required for correctness: the underlying `cosmos/iavl` LRU cache calls `c.ll.MoveToFront(ele)` on every `Get` — a linked-list mutation. `Has()` using `RLock` is also correct because `lruCache.Has()` only reads the map.

Block execution is single-threaded, so the lock overhead is an uncontended ~15–25 ns per call — negligible compared to the mmap reads and GC pressure from `string(key)` conversion in `lruCache.Get()`.

The `string(key)` conversion (heap allocation per cache lookup) is a more meaningful optimization target if this path is profiled hot.

---

### 9. Spin-loop time.Sleep

**File:** `memiavl/db.go:514–523` and `:1027–1036`
**Status:** ⬜ Open

#### Problem
```go
for {
    if db.lastCommitInfo.Version == committedVersion { break }
    time.Sleep(time.Nanosecond)  // actually sleeps ~1 ms (OS timer granularity)
}
```
`time.Sleep(time.Nanosecond)` sleeps for the OS minimum (~1 ms on Linux, ~15 ms on older macOS) due to kernel timer granularity.

#### Impact
**Near-zero in practice:** fires only once per 1,000 blocks (snapshot interval), typically 0–1 iterations before the condition is met. Amortized cost: <5 µs/block.

#### Fix
Replace with `sync.Cond` signalling from the async WAL writer. Eliminates OS timer granularity dependency and wasted spin CPU.

---

### 10. snapshotName fmt.Sprintf

**File:** `memiavl/db.go:1064–1066`
**Status:** ⬜ Open

```go
func snapshotName(version int64) string {
    return fmt.Sprintf("%s%020d", SnapshotPrefix, version)
}
```
Called once per commit cycle. Replace with `strconv.FormatInt` + manual zero-padding. Low priority — not on hot path.

---

### 11. bytes.Compare pattern

**File:** `memiavl/mem_node.go:163–181`
**Status:** ⬜ Open

Inner-node path uses `bytes.Compare(key, node.key) == -1` where `< 0` would be slightly more idiomatic. Micro-optimization, compiler likely optimizes this already.

---

### 12. Iterator stack not pre-allocated

**File:** `memiavl/iterator.go:19–35`
**Status:** ⬜ Open

Iterator stack starts at capacity 1 and grows via `append`. For tree depth 30–40, this causes O(log log n) reallocations per iterator. Fix: `make([]Node, 0, 40)`.

---

### 13. MultiTree.Copy map rebuild

**File:** `memiavl/multitree.go:170–183`
**Status:** ⬜ Open

Rebuilds `treesByName` map on every background snapshot (every 1,000 blocks). Allocates ~30 string keys. Low impact — not hot path.

---

### 14. Exporter channel-based allocs

**File:** `memiavl/export.go:117–132`
**Status:** ⬜ Open

Each exported node is heap-allocated and sent over a buffered channel (size 32). During state-sync snapshot export, this creates millions of `ExportNode` allocations. Consider `sync.Pool` for `ExportNode`. Only affects snapshot export, not block execution.

---

### 15. WAL marshal allocation

**File:** `memiavl/db.go:1290–1302`
**Status:** ⬜ Open

`entry.data.Marshal()` allocates a new `[]byte` on every block commit for protobuf serialization. Fix: use `MarshalToSizedBuffer` with a pre-allocated growing buffer stored on the DB struct.

---

### 16. flush() unoptimised sort

**File:** `store/rootmulti/store.go:73–93`
**Status:** ⬜ Open

`changeSets` slice is not pre-allocated (no capacity hint from `len(rs.stores)`). Each `NamedChangeSet` is individually heap-allocated. `sort.SliceStable` allocates internally. Fix: pre-allocate with `make([]*memiavl.NamedChangeSet, 0, len(rs.stores))`.

---

### 17. Proof generation allocs

**File:** `memiavl/proof.go:111–148`
**Status:** ⬜ Open

Multiple `convertVarIntToBytes` calls each return new slices, and multiple `append` calls may trigger reallocation. For a path of depth 30, this creates ~150 small allocations. Only used for ABCI queries with `prove=true`.

---

## Architectural Notes

### What is done well
- **mmap-based snapshot**: zero-copy reads directly from memory-mapped files — excellent
- **COW version tracking** via `cowVersion`: clean snapshot isolation without full tree copying
- **Async WAL** with channel-based batching: amortizes fsync costs well
- **Sharded cache** (16 shards): appropriate for reducing lock contention
- **`PersistedNode.Hash()`**: zero-copy mmap read, never recomputes hash
- **WAL batch reuse** (`db.wbatch`): good allocation pattern
- **`nativebyteorder` build tag**: zero-deserialization node layout for production

### What breaks at 10× scale
1. **No historical DB caching** (#2): query latency grows linearly with query volume
2. **Per-read RocksDB ReadOptions** (#6): dominates versiondb latency at high QPS
3. **Iterator allocation** (#5): GC pressure during range scans at scale
