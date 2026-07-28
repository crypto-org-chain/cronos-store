package rootmulti

import (
	stderrors "errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/crypto-org-chain/cronos-store/store/cachemulti"
	"github.com/crypto-org-chain/cronos-store/store/memiavlstore"

	"cosmossdk.io/errors"
	log "cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/store/v2/listenkv"
	"github.com/cosmos/cosmos-sdk/store/v2/mem"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"
	"github.com/cosmos/cosmos-sdk/store/v2/rootmulti"
	"github.com/cosmos/cosmos-sdk/store/v2/transient"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

const defaultHistoricalDBCacheSize = 4

// historicalDBEntry is a cached read-only *memiavl.DB, ref-counted so it's
// only closed once every borrow() has a matching release().
type historicalDBEntry struct {
	version int64
	db      *memiavl.DB
	refs    int  // active borrows
	evicted bool // removed from the LRU index but still held by a borrow
}

// historicalDBCache is a small bounded LRU cache of read-only *memiavl.DB
// instances keyed by version.
type historicalDBCache struct {
	mu      sync.Mutex
	maxSize int
	entries []*historicalDBEntry // index 0 is most-recently used
	closed  bool
	loadSem chan struct{} // bounds concurrent slow-path loads to maxSize
	// bumped whenever the underlying directory is replaced, so a load that
	// started before the swap can't insert a DB mapped over the old files.
	generation uint64
}

func newHistoricalDBCache(maxSize int) *historicalDBCache {
	if maxSize <= 0 {
		maxSize = defaultHistoricalDBCacheSize
	}
	return &historicalDBCache{maxSize: maxSize, loadSem: make(chan struct{}, maxSize)}
}

// lookup returns the cached entry for version and moves it to the front
// (MRU), incrementing its ref count. Returns nil if not cached. Caller must
// hold c.mu.
func (c *historicalDBCache) lookup(version int64) *historicalDBEntry {
	for i, e := range c.entries {
		if e.version == version {
			e.refs++
			copy(c.entries[1:i+1], c.entries[0:i]) // move to front (MRU)
			c.entries[0] = e
			return e
		}
	}
	return nil
}

// borrow returns the cached entry for version, loading it via load() if it is
// not already cached. The caller MUST call release() when done.
func (c *historicalDBCache) borrow(version int64, load func() (*memiavl.DB, error)) (*historicalDBEntry, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("historicalDBCache: cache is closed")
	}
	if e := c.lookup(version); e != nil {
		c.mu.Unlock()
		return e, nil
	}
	generation := c.generation
	c.mu.Unlock()

	// Cap concurrent loads at maxSize so a burst of distinct-version queries
	// can't fan out unbounded fd/mmap usage; excess callers queue here.
	c.loadSem <- struct{}{}
	defer func() { <-c.loadSem }()

	// load outside the lock so slow I/O doesn't block other borrowers.
	db, err := load()
	if err != nil {
		return nil, err
	}

	// close db if we return without caching it (closed, or another goroutine won the race).
	dbInserted := false
	defer func() {
		if !dbInserted {
			_ = db.Close()
		}
	}()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("historicalDBCache: cache is closed")
	}

	// The directory was replaced while we were loading, so this DB may be mapped
	// over files the reload has already unlinked. Make the caller retry against
	// the new generation rather than caching it.
	if c.generation != generation {
		return nil, fmt.Errorf("historicalDBCache: store reloaded while loading version %d", version)
	}

	// another goroutine may have loaded the same version while we were doing I/O.
	if e := c.lookup(version); e != nil {
		return e, nil
	}

	if len(c.entries) >= c.maxSize {
		oldest := c.entries[len(c.entries)-1]
		c.entries = c.entries[:len(c.entries)-1]
		oldest.evicted = true
		if oldest.refs == 0 {
			_ = oldest.db.Close()
		}
		// if refs > 0, release() closes it once the last borrower is done.
	}

	entry := &historicalDBEntry{version: version, db: db, refs: 1}
	c.entries = append([]*historicalDBEntry{entry}, c.entries...) // prepend as MRU
	dbInserted = true
	return entry, nil
}

// release decrements the ref count and closes the DB if it was evicted and
// this was the last borrow.
func (c *historicalDBCache) release(e *historicalDBEntry) {
	if e == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.refs <= 0 {
		panic(fmt.Sprintf("historicalDBCache: release called on entry with refs=%d", e.refs))
	}
	e.refs--
	if e.refs == 0 && e.evicted {
		_ = e.db.Close()
	}
}

// close drains the cache; entries still borrowed are closed by release() once
// their last borrower is done.
func (c *historicalDBCache) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.evictAllLocked()
}

// invalidate drains the cache because the directory its DBs are mapped over is
// being replaced. The generation bump makes an in-flight load discard its result
// instead of caching a DB mapped over the files the reload is unlinking.
func (c *historicalDBCache) invalidate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	return c.evictAllLocked()
}

// evictAllLocked marks every entry evicted and closes the ones nobody is
// borrowing. Caller must hold c.mu.
func (c *historicalDBCache) evictAllLocked() error {
	var errs []error
	for _, e := range c.entries {
		e.evicted = true
		if e.refs == 0 {
			if err := e.db.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	c.entries = nil
	return stderrors.Join(errs...)
}

// loadAtVersion loads a read-only memiavl DB pinned to version, rejecting
// memiavl.Load's silent fallback to the latest reachable version.
func loadAtVersion(dir string, opts memiavl.Options, chainId string, version int64) (*memiavl.DB, error) {
	opts.TargetVersion = uint32(version)
	opts.ReadOnly = true
	db, err := memiavl.Load(dir, opts, chainId)
	if err != nil {
		return nil, err
	}
	if actual := db.Version(); actual != version {
		_ = db.Close()
		return nil, fmt.Errorf("failed to load state at height %d; latest height is %d", version, actual)
	}
	return db, nil
}

// querySnapshot is an immutable copy-on-write view of committed state, read by
// latest-height query paths instead of the live rs.db/rs.stores that Commit
// mutates in place.
type querySnapshot struct {
	db             *memiavl.DB
	lastCommitInfo *types.CommitInfo
}

const CommitInfoFileName = "commit_infos"

var (
	_ types.CommitMultiStore = (*Store)(nil)
	_ types.Queryable        = (*Store)(nil)
)

type Store struct {
	dir     string
	db      *memiavl.DB
	logger  log.Logger
	chainId string

	// to keep it compatible with cosmos-sdk 0.46, merge the memstores into commit info
	lastCommitInfo *types.CommitInfo

	storesParams map[types.StoreKey]storeParams
	keysByName   map[string]types.StoreKey
	stores       map[types.StoreKey]types.CommitStore
	listeners    map[types.StoreKey]*types.MemoryListener

	opts memiavl.Options

	// sdk46Compact defines if the root hash is compatible with cosmos-sdk 0.46 and before.
	sdk46Compact bool
	// it's more efficient to export snapshot versions, we can filter out the non-snapshot versions
	supportExportNonSnapshotVersion bool

	historicalDBCache *historicalDBCache

	// published by publishQuerySnapshot after each state change.
	querySnapshot atomic.Pointer[querySnapshot]
}

func NewStore(dir string, logger log.Logger, sdk46Compact, supportExportNonSnapshotVersion bool, chainId string) *Store {
	return &Store{
		dir:                             dir,
		logger:                          logger,
		sdk46Compact:                    sdk46Compact,
		supportExportNonSnapshotVersion: supportExportNonSnapshotVersion,

		storesParams: make(map[types.StoreKey]storeParams),
		keysByName:   make(map[string]types.StoreKey),
		stores:       make(map[types.StoreKey]types.CommitStore),
		listeners:    make(map[types.StoreKey]*types.MemoryListener),
		chainId:      chainId,

		historicalDBCache: newHistoricalDBCache(defaultHistoricalDBCacheSize),
	}
}

// publishQuerySnapshot publishes an immutable view of the committed state and
// repoints the mounted iavl stores at it.
//
// The mounted stores must read the snapshot's trees, not rs.db's: rs.flush()
// applies change sets to the live trees in place, while readers branched off
// rs.stores (baseapp's CheckTx state, for one) can still be calling Get on them
// from another ABCI connection. Copy() bumps the source's copy-on-write version,
// so the snapshot's trees are never mutated underneath those readers.
//
// The publish is not atomic across stores: a concurrent CheckTx reader can see
// one mounted store already on the new version while another is still on the
// old one. Consensus state is unaffected — block execution runs on this
// goroutine, after the publish — and a torn view only costs CheckTx an
// occasional stale read.
func (rs *Store) publishQuerySnapshot() {
	db := rs.db.Copy()
	rs.querySnapshot.Store(&querySnapshot{
		db:             db,
		lastCommitInfo: rs.lastCommitInfo,
	})

	for key, store := range rs.stores {
		memiavlStore, ok := store.(*memiavlstore.Store)
		if !ok {
			continue
		}
		tree := db.TreeByName(key.Name())
		if tree == nil {
			panic(fmt.Sprintf("no memiavl tree for mounted store: %s", key.Name()))
		}
		memiavlStore.SetTree(tree)
	}
}

func (rs *Store) latestDB() *memiavl.DB {
	if snap := rs.querySnapshot.Load(); snap != nil {
		return snap.db
	}
	return rs.db
}

func (rs *Store) latestCommitInfo() *types.CommitInfo {
	if snap := rs.querySnapshot.Load(); snap != nil {
		return snap.lastCommitInfo
	}
	return rs.lastCommitInfo
}

func (rs *Store) refreshLastCommitInfo(db *memiavl.DB) *types.CommitInfo {
	info := convertCommitInfo(db.LastCommitInfo())
	if rs.sdk46Compact {
		info = amendCommitInfo(info, rs.storesParams)
	}
	return info
}

// rebuildStores loads a CommitStore for every key in keys; keys must be
// pre-sorted for deterministic iteration order.
func (rs *Store) rebuildStores(db *memiavl.DB, keys []types.StoreKey) (map[types.StoreKey]types.CommitStore, error) {
	newStores := make(map[types.StoreKey]types.CommitStore, len(keys))
	for _, key := range keys {
		store, err := rs.loadCommitStoreFromParams(db, key, rs.storesParams[key])
		if err != nil {
			return nil, err
		}
		newStores[key] = store
	}
	return newStores, nil
}

// flush writes all the pending change sets to memiavl tree.
func (rs *Store) flush() error {
	var changeSets []*memiavl.NamedChangeSet
	for key := range rs.stores {
		// it'll unwrap the inter-block cache
		store := rs.GetCommitStore(key)
		if memiavlStore, ok := store.(*memiavlstore.Store); ok {
			cs := memiavlStore.PopChangeSet()
			if len(cs.Pairs) > 0 {
				changeSets = append(changeSets, &memiavl.NamedChangeSet{
					Name:      key.Name(),
					Changeset: cs,
				})
			}
		}
	}
	sort.SliceStable(changeSets, func(i, j int) bool {
		return changeSets[i].Name < changeSets[j].Name
	})

	return rs.db.ApplyChangeSets(changeSets)
}

// WorkingHash returns the app hash of the working tree,
//
// Implements interface Committer.
func (rs *Store) WorkingHash() []byte {
	if err := rs.flush(); err != nil {
		panic(err)
	}
	commitInfo := convertCommitInfo(rs.db.WorkingCommitInfo())
	if rs.sdk46Compact {
		commitInfo = amendCommitInfo(commitInfo, rs.storesParams)
	}
	return commitInfo.Hash()
}

// Commit Implements interface Committer
func (rs *Store) Commit() types.CommitID {
	if err := rs.flush(); err != nil {
		panic(err)
	}

	for _, store := range rs.stores {
		if store.GetStoreType() != types.StoreTypeIAVL {
			_ = store.Commit()
		}
	}

	_, err := rs.db.Commit()
	if err != nil {
		panic(err)
	}

	rs.lastCommitInfo = rs.refreshLastCommitInfo(rs.db)
	// also repoints the mounted stores' trees, which db.Commit may have reloaded.
	rs.publishQuerySnapshot()
	return rs.lastCommitInfo.CommitID()
}

func (rs *Store) Close() error {
	return stderrors.Join(rs.closeDB(), rs.historicalDBCache.close())
}

// closeDB unpublishes and closes the current db, dropping the commit info that
// describes it. No-op when there is no db: rs.db is nil on a repeat Close, or
// after a failed Restore or RollbackToVersion.
func (rs *Store) closeDB() error {
	rs.dropQuerySnapshot()
	if rs.db == nil {
		return nil
	}

	err := rs.db.Close()
	// nil rs.db right after Close so no later path reuses the closed handle.
	rs.db = nil
	// Drop the commit info with the db it describes: otherwise LastCommitID keeps
	// reporting a version whose data is gone or about to be replaced.
	rs.lastCommitInfo = nil
	return err
}

// dropQuerySnapshot clears the published snapshot before rs.db is closed: it
// shares rs.db's mmap'd state, so closing rs.db first leaves a window where a
// reader loads a snapshot backed by unmapped memory.
func (rs *Store) dropQuerySnapshot() {
	rs.querySnapshot.Store(nil)
}

// closeDBForReload closes the current db and drops the cached historical ones so
// the caller can load a replacement over the same directory.
//
// A Close error is logged rather than returned: memiavl.DB.Close runs the rest of its
// cleanup regardless, and its WAL error latch is sticky, so propagating it would leave
// rs.db pointing at a closed handle and wedge every later retry on the same error. The
// unsynced WAL tail such an error implies is exactly what a rollback or restore discards.
func (rs *Store) closeDBForReload() {
	if err := rs.closeDB(); err != nil {
		rs.logger.Error("failed to close memiavl db before reload", "err", err)
	}
	// Cached historical DBs stay mmap'd over files the reload unlinks, so they would
	// keep answering queries - with proofs - out of a history the node no longer has.
	if err := rs.historicalDBCache.invalidate(); err != nil {
		rs.logger.Error("failed to close cached historical memiavl dbs before reload", "err", err)
	}
}

// LastCommitID Implements interface Committer
//
// With no published snapshot - before the first load, or after a Close or a failed
// reload - the version is read back from disk and the returned CommitID carries no
// hash. Reporting version 0 instead would tell CometBFT to replay from genesis.
func (rs *Store) LastCommitID() types.CommitID {
	lastCommitInfo := rs.latestCommitInfo()
	if lastCommitInfo == nil {
		v, err := memiavl.GetLatestVersion(rs.dir)
		if err != nil {
			panic(fmt.Errorf("failed to get latest version: %w", err))
		}
		return types.CommitID{Version: v}
	}

	return lastCommitInfo.CommitID()
}

// SetPruning Implements interface Committer
func (rs *Store) SetPruning(pruningtypes.PruningOptions) {
}

// SetMetrics is a noop as metrics support was removed in store/v2
func (rs *Store) SetMetrics(_ interface{}) {
}

// GetPruning Implements interface Committer
func (rs *Store) GetPruning() pruningtypes.PruningOptions {
	return pruningtypes.NewPruningOptions(pruningtypes.PruningDefault)
}

// GetStoreType Implements interface Store
func (rs *Store) GetStoreType() types.StoreType {
	return types.StoreTypeMulti
}

// CacheWrap Implements interface CacheWrapper
func (rs *Store) CacheWrap() types.CacheWrap {
	return rs.CacheMultiStore().(types.CacheWrap)
}

// CacheWrapWithTrace Implements interface CacheWrapper
func (rs *Store) CacheWrapWithTrace(_ io.Writer, _ interface{}) types.CacheWrap {
	return rs.CacheWrap()
}

// CacheMultiStore Implements interface MultiStore.
//
// Must wrap the live rs.stores, not a querySnapshot: writes flow back through
// cachemulti's Write(), which a snapshot-backed store would silently discard.
func (rs *Store) CacheMultiStore() types.CacheMultiStore {
	stores := make(map[types.StoreKey]types.CacheWrapper, len(rs.stores))
	for k, v := range rs.stores {
		stores[k] = v
	}
	return cachemulti.NewStore(rs.wireListeners(stores), nil, nil, nil)
}

// wireListeners wraps listening-enabled stores in a listenkv.Store so listeners
// observe writes made through the cache store.
func (rs *Store) wireListeners(stores map[types.StoreKey]types.CacheWrapper) map[types.StoreKey]types.CacheWrapper {
	for k, store := range stores {
		if kv, ok := store.(types.KVStore); ok && rs.ListeningEnabled(k) {
			stores[k] = listenkv.NewStore(kv, k, rs.listeners[k])
		}
	}
	return stores
}

func (rs *Store) storesFromDB(db *memiavl.DB) map[types.StoreKey]types.CacheWrapper {
	stores := make(map[types.StoreKey]types.CacheWrapper)

	// add the transient/mem stores registered in current app.
	for k, store := range rs.stores {
		if store.GetStoreType() != types.StoreTypeIAVL {
			stores[k] = store
		}
	}

	// A historical snapshot may hold trees for stores a later StoreUpgrade
	// deleted or renamed; skip those, there's no current StoreKey for them.
	for _, tree := range db.Trees() {
		key, ok := rs.keysByName[tree.Name]
		if !ok {
			continue
		}
		stores[key] = memiavlstore.New(tree.Tree, rs.logger)
	}

	return stores
}

// cacheMultiStoreFromDB builds a CacheMultiStore from db's iavl trees. Pass
// closer=db for an independently-owned db, or nil for a query snapshot, which
// shares mmap state with rs.db and must never be closed on its own.
func (rs *Store) cacheMultiStoreFromDB(db *memiavl.DB, closer io.Closer) types.CacheMultiStore {
	return cachemulti.NewStore(rs.storesFromDB(db), nil, nil, closer)
}

// CacheMultiStoreWithVersion Implements interface MultiStore
// used to createQueryContext, abci_query or grpc query service.
//
// version == 0 means the latest committed snapshot, not the live working state.
// The snapshot-backed stores are read-only: nothing flushes their change sets,
// so a caller's Write() is discarded rather than committed.
func (rs *Store) CacheMultiStoreWithVersion(version int64) (types.CacheMultiStore, error) {
	snap := rs.querySnapshot.Load()
	if snap == nil {
		return nil, errors.Wrap(sdkerrors.ErrInvalidRequest, "store is not loaded")
	}
	if version == 0 || version == snap.lastCommitInfo.Version {
		return rs.cacheMultiStoreFromDB(snap.db, nil), nil
	}

	if version < 0 || version > math.MaxUint32 {
		return nil, fmt.Errorf("version out of range: %d", version)
	}
	// historicalDBCache isn't used here: the caller owns the returned store's
	// lifetime (closed via the closer arg), so the cache's borrow/release model doesn't fit.
	db, err := loadAtVersion(rs.dir, rs.opts, rs.chainId, version)
	if err != nil {
		return nil, err
	}

	return rs.cacheMultiStoreFromDB(db, db), nil
}

// GetStore Implements interface MultiStore
func (rs *Store) GetStore(key types.StoreKey) types.Store {
	s, ok := rs.stores[key]
	if !ok {
		panic(fmt.Sprintf("store does not exist for key: %s", key.Name()))
	}
	return s
}

// GetKVStore Implements interface MultiStore
func (rs *Store) GetKVStore(key types.StoreKey) types.KVStore {
	s, ok := rs.GetStore(key).(types.KVStore)
	if !ok {
		panic(fmt.Sprintf("store with key %v is not KVStore", key))
	}
	return s
}

// TracingEnabled Implements interface MultiStore
func (rs *Store) TracingEnabled() bool {
	return false
}

// SetTracer Implements interface MultiStore
func (rs *Store) SetTracer(_ io.Writer) types.MultiStore {
	return rs
}

// SetTracingContext Implements interface MultiStore
func (rs *Store) SetTracingContext(_ interface{}) types.MultiStore {
	return rs
}

// LatestVersion Implements interface MultiStore
func (rs *Store) LatestVersion() int64 {
	db := rs.latestDB()
	if db == nil {
		// Restore closed the db and LoadLatestVersion hasn't run yet.
		return 0
	}
	return db.Version()
}

// EarliestVersion Implements interface CommitMultiStore
func (rs *Store) EarliestVersion() int64 {
	db := rs.latestDB()
	if db == nil {
		return 0
	}
	// memiavl prunes WAL entries up to the earliest retained snapshot, so the
	// earliest queryable version is the version of that snapshot.
	v, err := db.EarliestVersion()
	if err != nil {
		rs.logger.Error("failed to get earliest version", "err", err)
		return 0
	}
	return v
}

// PruneSnapshotHeight Implements interface Snapshotter
// not needed, memiavl manage its own snapshot/pruning strategy
func (rs *Store) PruneSnapshotHeight(height int64) {
}

// SetSnapshotInterval Implements interface Snapshotter
// not needed, memiavl manage its own snapshot/pruning strategy
func (rs *Store) SetSnapshotInterval(snapshotInterval uint64) {
}

// MountStoreWithDB Implements interface CommitMultiStore
func (rs *Store) MountStoreWithDB(key types.StoreKey, typ types.StoreType, _ dbm.DB) {
	if key == nil {
		panic("MountIAVLStore() key cannot be nil")
	}
	if _, ok := rs.storesParams[key]; ok {
		panic(fmt.Sprintf("store duplicate store key %v", key))
	}
	if _, ok := rs.keysByName[key.Name()]; ok {
		panic(fmt.Sprintf("store duplicate store key name %v", key))
	}
	rs.storesParams[key] = newStoreParams(key, typ)
	rs.keysByName[key.Name()] = key
}

// GetCommitStore Implements interface CommitMultiStore
func (rs *Store) GetCommitStore(key types.StoreKey) types.CommitStore {
	return rs.stores[key]
}

// GetCommitKVStore Implements interface CommitMultiStore
func (rs *Store) GetCommitKVStore(key types.StoreKey) types.CommitKVStore {
	store, ok := rs.GetCommitStore(key).(types.CommitKVStore)
	if !ok {
		panic(fmt.Sprintf("store with key %v is not CommitKVStore", key))
	}

	return store
}

// LoadLatestVersion Implements interface CommitMultiStore
// used by normal node startup.
func (rs *Store) LoadLatestVersion() error {
	return rs.LoadVersionAndUpgrade(0, nil)
}

// LoadLatestVersionAndUpgrade Implements interface CommitMultiStore
func (rs *Store) LoadLatestVersionAndUpgrade(upgrades *types.StoreUpgrades) error {
	return rs.LoadVersionAndUpgrade(0, upgrades)
}

// LoadVersionAndUpgrade Implements interface CommitMultiStore
// used by node startup with UpgradeStoreLoader
func (rs *Store) LoadVersionAndUpgrade(version int64, upgrades *types.StoreUpgrades) error {
	if version > math.MaxUint32 {
		return fmt.Errorf("version overflows uint32: %d", version)
	}

	storesKeys := make([]types.StoreKey, 0, len(rs.storesParams))
	for key := range rs.storesParams {
		storesKeys = append(storesKeys, key)
	}
	// deterministic iteration order for upgrades
	sort.Slice(storesKeys, func(i, j int) bool {
		return storesKeys[i].Name() < storesKeys[j].Name()
	})

	initialStores := make([]string, 0, len(storesKeys))
	for _, key := range storesKeys {
		if rs.storesParams[key].typ == types.StoreTypeIAVL {
			initialStores = append(initialStores, key.Name())
		}
	}

	opts := rs.opts
	opts.CreateIfMissing = true
	opts.InitialStores = initialStores
	opts.TargetVersion = uint32(version)
	// A db already loaded over rs.dir holds the directory lock, so a reload has to
	// give it up first or memiavl.Load fails on the flock.
	rs.closeDBForReload()
	db, err := memiavl.Load(rs.dir, opts, rs.chainId)
	if err != nil {
		return errors.Wrapf(err, "fail to load memiavl at %s", rs.dir)
	}
	dbInstalled := false
	defer func() {
		if !dbInstalled {
			_ = db.Close()
		}
	}()

	var treeUpgrades []*memiavl.TreeNameUpgrade
	if upgrades != nil {
		for _, name := range upgrades.Deleted {
			treeUpgrades = append(treeUpgrades, &memiavl.TreeNameUpgrade{Name: name, Delete: true})
		}
		for _, name := range upgrades.Added {
			treeUpgrades = append(treeUpgrades, &memiavl.TreeNameUpgrade{Name: name})
		}
		for _, rename := range upgrades.Renamed {
			treeUpgrades = append(treeUpgrades, &memiavl.TreeNameUpgrade{Name: rename.NewKey, RenameFrom: rename.OldKey})
		}
	}

	if len(treeUpgrades) > 0 {
		if err := db.ApplyUpgrades(treeUpgrades); err != nil {
			return err
		}
	}

	// Validate latest and post-upgrade membership. Historical loads may
	// legitimately contain stores that were deleted or renamed later.
	if version == 0 || len(treeUpgrades) > 0 {
		if err := validateMemiAVLTreeMembership(db, initialStores); err != nil {
			return err
		}
	}

	newStores, err := rs.rebuildStores(db, storesKeys)
	if err != nil {
		return err
	}

	rs.db = db
	dbInstalled = true
	rs.stores = newStores
	// to keep the root hash compatible with cosmos-sdk 0.46
	if db.Version() != 0 {
		rs.lastCommitInfo = rs.refreshLastCommitInfo(db)
	} else {
		rs.lastCommitInfo = &types.CommitInfo{}
	}
	rs.publishQuerySnapshot()

	return nil
}

func validateMemiAVLTreeMembership(db *memiavl.DB, expectedNames []string) error {
	expected := make(map[string]struct{}, len(expectedNames))
	for _, name := range expectedNames {
		expected[name] = struct{}{}
	}

	unexpected := make([]string, 0)
	for _, tree := range db.Trees() {
		if _, ok := expected[tree.Name]; ok {
			delete(expected, tree.Name)
			continue
		}
		unexpected = append(unexpected, tree.Name)
	}

	missing := make([]string, 0, len(expected))
	for name := range expected {
		missing = append(missing, name)
	}

	if len(missing) > 0 || len(unexpected) > 0 {
		// The map iteration order is undefined, so sort missing for deterministic
		// error presentation. unexpected follows db.Trees(), which is ordered by name.
		sort.Strings(missing)
		return fmt.Errorf("memiavl tree membership mismatch: missing=%v unexpected=%v", missing, unexpected)
	}
	return nil
}

func (rs *Store) loadCommitStoreFromParams(db *memiavl.DB, key types.StoreKey, params storeParams) (types.CommitStore, error) {
	switch params.typ {
	case types.StoreTypeMulti:
		panic("recursive MultiStores not yet supported")
	case types.StoreTypeIAVL:
		tree := db.TreeByName(key.Name())
		if tree == nil {
			return nil, fmt.Errorf("new store is not added in upgrades: %s", key.Name())
		}
		return types.CommitStore(memiavlstore.New(tree, rs.logger)), nil
	case types.StoreTypeDB:
		panic("recursive MultiStores not yet supported")
	case types.StoreTypeTransient:
		if _, ok := key.(*types.TransientStoreKey); !ok {
			return nil, fmt.Errorf("unexpected key type for a TransientStoreKey; got: %s, %T", key.String(), key)
		}

		return transient.NewStore(), nil

	case types.StoreTypeMemory:
		if _, ok := key.(*types.MemoryStoreKey); !ok {
			return nil, fmt.Errorf("unexpected key type for a MemoryStoreKey; got: %s", key.String())
		}

		return mem.NewStore(), nil

	default:
		return rs.loadExtraStore(db, key, params)
	}
}

// LoadVersion Implements interface CommitMultiStore
// used by export cmd
func (rs *Store) LoadVersion(ver int64) error {
	return rs.LoadVersionAndUpgrade(ver, nil)
}

// SetInterBlockCache is a noop here because memiavl do caching on it's own, which works well with zero-copy.
func (rs *Store) SetInterBlockCache(c types.MultiStorePersistentCache) {}

// SetInitialVersion Implements interface CommitMultiStore
// used by InitChain when the initial height is bigger than 1
func (rs *Store) SetInitialVersion(version int64) error {
	if err := rs.db.SetInitialVersion(version); err != nil {
		return err
	}
	// keep the published snapshot from going stale against the mutated rs.db.
	rs.publishQuerySnapshot()
	return nil
}

// SetIAVLCacheSize Implements interface CommitMultiStore
func (rs *Store) SetIAVLCacheSize(size int) {
}

// SetIAVLDisableFastNode Implements interface CommitMultiStore
func (rs *Store) SetIAVLDisableFastNode(disable bool) {
}

// SetIAVLSyncPruning Implements interface CommitMultiStore
func (rs *Store) SetIAVLSyncPruning(syncPruning bool) {
}

// SetLazyLoading Implements interface CommitMultiStore
func (rs *Store) SetLazyLoading(lazyLoading bool) {
}

func (rs *Store) SetMemIAVLOptions(opts memiavl.Options) {
	if opts.Logger == nil {
		opts.Logger = memiavl.Logger(rs.logger.With("module", "memiavl"))
	}
	rs.opts = opts
}

// RollbackToVersion delete the versions after `target` and update the latest version.
// it should only be called in standalone cli commands: it closes rs.db outright,
// invalidating any querySnapshot published before this call.
func (rs *Store) RollbackToVersion(target int64) error {
	if target <= 0 {
		return fmt.Errorf("invalid rollback height target: %d", target)
	}

	if target > math.MaxUint32 {
		return fmt.Errorf("rollback height target %d exceeds max uint32", target)
	}

	rs.closeDBForReload()

	opts := rs.opts
	opts.TargetVersion = uint32(target)
	opts.LoadForOverwriting = true

	db, err := memiavl.Load(rs.dir, opts, rs.chainId)
	if err != nil {
		return err
	}

	// Rebuild rs.stores before swapping rs.db in: the old entries hold trees from
	// the just-closed, unmapped db.
	keys := make([]types.StoreKey, 0, len(rs.storesParams))
	for key := range rs.storesParams {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name() < keys[j].Name() })

	newStores, err := rs.rebuildStores(db, keys)
	if err != nil {
		_ = db.Close()
		return err
	}
	rs.db = db
	rs.stores = newStores

	// resync lastCommitInfo, so the snapshot's version checks don't compare
	// against the pre-rollback version.
	rs.lastCommitInfo = rs.refreshLastCommitInfo(rs.db)
	rs.publishQuerySnapshot()

	return nil
}

// ListeningEnabled Implements interface CommitMultiStore
func (rs *Store) ListeningEnabled(key types.StoreKey) bool {
	if ls, ok := rs.listeners[key]; ok {
		return ls != nil
	}
	return false
}

// AddListeners Implements interface CommitMultiStore
func (rs *Store) AddListeners(keys []types.StoreKey) {
	for i := range keys {
		listener := rs.listeners[keys[i]]
		if listener == nil {
			rs.listeners[keys[i]] = types.NewMemoryListener()
		}
	}
}

// PopStateCache returns the accumulated state change messages from the CommitMultiStore
// Calling PopStateCache destroys only the currently accumulated state in each listener
// not the state in the store itself. This is a mutating and destructive operation.
// This method has been synchronized.
func (rs *Store) PopStateCache() []*types.StoreKVPair {
	var cache []*types.StoreKVPair
	for key := range rs.listeners {
		ls := rs.listeners[key]
		if ls != nil {
			cache = append(cache, ls.PopStateCache()...)
		}
	}
	sort.SliceStable(cache, func(i, j int) bool {
		return cache[i].StoreKey < cache[j].StoreKey
	})
	return cache
}

// GetStoreByName performs a lookup of a StoreKey given a store name typically
// provided in a path. The StoreKey is then used to perform a lookup and return
// a Store. If the Store is wrapped in an inter-block cache, it will be unwrapped
// prior to being returned. If the StoreKey does not exist, nil is returned.
func (rs *Store) GetStoreByName(name string) types.Store {
	key := rs.keysByName[name]
	if key == nil {
		return nil
	}

	return rs.GetCommitStore(key)
}

// Query Implements interface Queryable
func (rs *Store) Query(req *types.RequestQuery) (*types.ResponseQuery, error) {
	snap := rs.querySnapshot.Load()

	version := req.Height
	if version == 0 {
		db := rs.db
		if snap != nil {
			db = snap.db
		}
		if db == nil {
			return nil, errors.Wrap(sdkerrors.ErrInvalidRequest, "store is not loaded")
		}
		version = db.Version()
	}

	if version < 0 || version > math.MaxUint32 {
		return nil, fmt.Errorf("version out of range: %d", version)
	}

	// At the latest height, read the snapshot instead of the live rs.db that
	// Commit mutates in place. Otherwise load from disk.
	var db *memiavl.DB
	var borrowedEntry *historicalDBEntry
	if snap != nil && version == snap.lastCommitInfo.Version {
		db = snap.db
	} else {
		var err error
		borrowedEntry, err = rs.historicalDBCache.borrow(version, func() (*memiavl.DB, error) {
			return loadAtVersion(rs.dir, rs.opts, rs.chainId, version)
		})
		if err != nil {
			return nil, err
		}
		defer rs.historicalDBCache.release(borrowedEntry)
		db = borrowedEntry.db
	}

	path := req.Path
	storeName, subpath, err := parsePath(path)
	if err != nil {
		return nil, err
	}

	// db may be a historical snapshot whose store set differs from the
	// currently-mounted keysByName (e.g. stores deleted or renamed by a
	// later upgrade), so the snapshot's own tree set is authoritative.
	tree := db.TreeByName(storeName)
	if tree == nil {
		if _, ok := rs.keysByName[storeName]; !ok {
			return nil, errors.Wrapf(sdkerrors.ErrUnknownRequest, "no such store: %s", storeName)
		}
		return nil, errors.Wrapf(sdkerrors.ErrUnknownRequest, "store %s not present in historical snapshot at this version", storeName)
	}

	store := types.Queryable(memiavlstore.New(tree, rs.logger))

	// trim the path and make the query
	req.Path = subpath
	res, err := store.Query(req)
	if err != nil {
		return nil, err
	}

	if !req.Prove || !rootmulti.RequireProof(subpath) {
		return res, nil
	}

	if res.ProofOps == nil || len(res.ProofOps.Ops) == 0 {
		return nil, errors.Wrap(sdkerrors.ErrInvalidRequest, "proof is unexpectedly empty; ensure height has not been pruned")
	}

	// reuse the snapshot's commit info instead of paying refreshLastCommitInfo's
	// per-store conversion on every proved query.
	var commitInfo *types.CommitInfo
	if snap != nil && db == snap.db {
		commitInfo = snap.lastCommitInfo
	} else {
		commitInfo = rs.refreshLastCommitInfo(db)
	}

	// Restore origin path and append proof op.
	res.ProofOps.Ops = append(res.ProofOps.Ops, commitInfo.ProofOp(storeName))

	return res, nil
}

// parsePath expects a format like /<storeName>[/<subpath>]
// Must start with /, subpath may be empty
// Returns error if it doesn't start with /
func parsePath(path string) (storeName, subpath string, err error) {
	if !strings.HasPrefix(path, "/") {
		return storeName, subpath, errors.Wrapf(sdkerrors.ErrUnknownRequest, "invalid path: %s", path)
	}

	paths := strings.SplitN(path[1:], "/", 2)
	storeName = paths[0]

	if len(paths) == 2 {
		subpath = "/" + paths[1]
	}

	return storeName, subpath, nil
}

type storeParams struct {
	key types.StoreKey
	typ types.StoreType
}

func newStoreParams(key types.StoreKey, typ types.StoreType) storeParams {
	return storeParams{
		key: key,
		typ: typ,
	}
}

func mergeStoreInfos(commitInfo *types.CommitInfo, storeInfos []types.StoreInfo) *types.CommitInfo {
	infos := make([]types.StoreInfo, 0, len(commitInfo.StoreInfos)+len(storeInfos))
	infos = append(infos, commitInfo.StoreInfos...)
	infos = append(infos, storeInfos...)
	sort.SliceStable(infos, func(i, j int) bool {
		return infos[i].Name < infos[j].Name
	})
	return &types.CommitInfo{
		Version:    commitInfo.Version,
		StoreInfos: infos,
	}
}

// amendCommitInfo add mem stores commit infos to keep it compatible with cosmos-sdk 0.46
func amendCommitInfo(commitInfo *types.CommitInfo, storeParams map[types.StoreKey]storeParams) *types.CommitInfo {
	var extraStoreInfos []types.StoreInfo
	for key := range storeParams {
		typ := storeParams[key].typ
		if typ != types.StoreTypeIAVL && typ != types.StoreTypeTransient {
			extraStoreInfos = append(extraStoreInfos, types.StoreInfo{
				Name:     key.Name(),
				CommitId: types.CommitID{},
			})
		}
	}
	return mergeStoreInfos(commitInfo, extraStoreInfos)
}

func convertCommitInfo(commitInfo *memiavl.CommitInfo) *types.CommitInfo {
	storeInfos := make([]types.StoreInfo, len(commitInfo.StoreInfos))
	for i, storeInfo := range commitInfo.StoreInfos {
		storeInfos[i] = types.StoreInfo{
			Name: storeInfo.Name,
			CommitId: types.CommitID{
				Version: storeInfo.CommitId.Version,
				Hash:    storeInfo.CommitId.Hash,
			},
		}
	}
	return &types.CommitInfo{
		Version:    commitInfo.Version,
		StoreInfos: storeInfos,
	}
}
