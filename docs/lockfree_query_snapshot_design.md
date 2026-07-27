# Lock-free query snapshots for rootmulti.Store

## Problem

`store/rootmulti/store.go` guards `Commit()`/`Query()`/`CacheMultiStore*` with a
single `rs.mtx sync.RWMutex` (PR #77 and its predecessor). Two problems remain:

1. **The lock doesn't cover the actual race.** `CacheMultiStore()`,
   `CacheMultiStoreWithVersion()`, and `GetKVStore()` return a handle that
   holds a live reference to the same `*memiavlstore.Store` objects
   `Commit()` mutates via `SetTree`. The `RLock` is released before the
   caller ever calls `Get()`/`Iterator()` on that handle, so the read races
   unsynchronized against the next `Commit()`'s `flush()`. Confirmed with
   `go test -race -count=80` on the PR's own concurrency test.
2. **The lock is expensive.** `Commit()`/`WorkingHash()` hold the exclusive
   lock across `flush()` + `db.Commit()` — real disk I/O, potentially
   hundreds of ms on a snapshot-rewrite block. Every reader (`Query` at
   latest height, `LatestVersion`, `EarliestVersion`, `CacheMultiStore`)
   now stalls for that whole window, which didn't happen before (the old
   code was racy but non-blocking). In the other direction, `Query()` with
   `Prove: true` holds `RLock` through proof generation, which can delay
   `Commit()` acquiring `Lock()` — a query-driven way to slow down block
   production.

Root cause: `memiavl.Tree` mutates its nodes in place, and rootmulti hands
out direct references to that live, mutable state. A mutex at the rootmulti
layer can only ever protect the moment of taking the reference, not what the
caller does with it afterward.

## The mechanism already exists in memiavl

`memiavl.Tree` already supports copy-on-write:

- `Tree.Copy(cacheSize int) *Tree` (`memiavl/tree.go:237`) sets
  `t.cowVersion = t.version` on the original tree and returns a new `*Tree`
  sharing the same root. From that point on, `setRecursive`/`removeRecursive`
  (`memiavl/node.go`) call `node.Mutate(version, cowVersion)`, which clones
  any node with `node.version <= cowVersion` instead of mutating it in
  place. So the original tree can keep being written by `Commit()` while the
  copy stays exactly as it was — no shared mutable state between them.
- `MultiTree.Copy` / `DB.Copy()` (`memiavl/db.go:733`) do the same thing for
  every tree in a `DB`, under `db.mtx` briefly (cheap: proportional to the
  number of stores, not tree size).

This is the same primitive `db_test.go` uses for snapshot isolation. It is
**not currently used** on the hot read path in `rootmulti.Store` — readers go
through `rs.db`/`rs.stores` directly instead of a copy.

## Proposed design

Publish an immutable, ready-to-read snapshot once per block instead of
locking readers against the live, mutating state:

1. Add `rs.querySnapshot atomic.Pointer[querySnapshot]` to `Store`, where
   `querySnapshot` holds `{db *memiavl.DB, lastCommitInfo *types.CommitInfo}`.
2. At the end of `Commit()` (after `flush()`, `rs.db.Commit()`, and the
   `SetTree` loop), call `rs.db.Copy()` once and
   `rs.querySnapshot.Store(&querySnapshot{db: snap, lastCommitInfo: ...})`.
   This is the only place a snapshot is taken — once per block, not once
   per query, so the cache-allocation cost of `Copy()` is amortized over
   every query in that block.
3. `Query()` (latest-height path), `CacheMultiStore()`,
   `CacheMultiStoreWithVersion()` (version==0 or version==latest),
   `LatestVersion()`, `EarliestVersion()` all read
   `rs.querySnapshot.Load()` instead of `rs.db`/`rs.stores`/
   `rs.lastCommitInfo`. This is a single atomic load — no `rs.mtx` needed
   for any of these paths at all.
4. `rs.mtx` (or a narrower lock) is only needed to serialize `Commit()`
   against itself and against the rare non-query mutators
   (`LoadVersionAndUpgrade`, `RollbackToVersion`, `SetInitialVersion`) — it
   drops off the read path entirely.
5. Historical queries (`version != latest`) are unaffected — they already
   go through `historicalDBCache`, which loads an independent `*memiavl.DB`
   from disk.

Net effect: readers never block on Commit's disk I/O, Commit never blocks on
slow readers (no more `Prove: true` griefing vector against block timing),
and the CacheMultiStore-escapes-the-lock race is closed structurally instead
of by locking harder.

## Open questions to resolve before implementing

- **Does `DB.Copy()`'s result need `Close()`?** The cloned `DB` shares
  `snapshotWriterPool` and `dir` with the original but its `MultiTree` is an
  in-memory struct copy. Need to confirm the clone doesn't need explicit
  `Close()` (i.e. it holds no exclusively-owned file descriptors) — if it
  does, snapshots need a lifetime/refcount story instead of "let it get
  GC'd."
- **Snapshot retention**: only the latest snapshot needs to be reachable via
  the atomic pointer; older ones are kept alive only by in-flight readers
  still holding a reference (normal Go GC, no explicit pruning needed) —
  confirm this doesn't fight with memiavl's own snapshot/WAL pruning
  (`PruneSnapshotHeight`), which operates on-disk, not on these in-memory
  copies.
- **Cache size for the per-block copy**: `Tree.Copy` allocates a fresh
  `NewCache(cacheSize)` per tree. Once per block this is fine; verify the
  configured `cacheSize` doesn't make this allocation itself a per-block
  latency spike worth measuring.
- **WorkingHash() vs Commit()**: `WorkingHash()` already calls `flush()`;
  `Commit()`'s `flush()` is then a near no-op. Decide whether the snapshot
  should be taken after `WorkingHash()` (once ABCI's proposal hash is final)
  rather than duplicating work in `Commit()`.

## Migration path

This is a memiavl-adjacent change but lands entirely in
`store/rootmulti/store.go` — no memiavl API changes needed, since `Copy()`
is already public. Land it as its own PR, with the same race test from
PR #77 (`TestCacheMultiStoreWithVersionExactVersionRace`) extended to also
exercise `CacheMultiStore()`/`GetKVStore()` directly under `-race`, since
those are the paths PR #77 left open (see short-term mitigation below).
