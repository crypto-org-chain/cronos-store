package memiavlstore

import (
	"io"
	"runtime"
	"slices"
	"sync/atomic"

	cmtprotocrypto "github.com/cometbft/cometbft/proto/tendermint/crypto"
	ics23 "github.com/cosmos/ics23/go"
	"github.com/crypto-org-chain/cronos-store/memiavl"

	"cosmossdk.io/errors"
	log "cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/store/v2/cachekv"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var (
	_ types.KVStore       = (*Store)(nil)
	_ types.CommitStore   = (*Store)(nil)
	_ types.CommitKVStore = (*Store)(nil)
	_ types.Queryable     = (*Store)(nil)
)

// Store Implements types.KVStore and CommitKVStore.
type Store struct {
	// set for a mounted store, which reads its tree from mount's current view.
	mount *Mount
	name  string
	// where the last view held this store's tree; a hint, checked by name on every read.
	pos atomic.Int64

	tree   *memiavl.Tree
	logger log.Logger

	changeSet memiavl.ChangeSet
}

func New(tree *memiavl.Tree, logger log.Logger) *Store {
	return &Store{tree: tree, logger: logger}
}

// NewMounted returns a store that reads the tree named name from mount's view.
func NewMounted(mount *Mount, name string, logger log.Logger) *Store {
	return &Store{mount: mount, name: name, logger: logger}
}

func (st *Store) SetTree(tree *memiavl.Tree) {
	if st.mount != nil {
		panic("memiavlstore: SetTree on a mounted store, which reads its mount's view")
	}
	st.tree = tree
}

// View is one set of trees that the mounted stores read together.
type View struct {
	trees []memiavl.NamedTree
}

// NewView takes db, a copy holding its snapshots' references, and closes it once
// the view is unreachable; readers keep the view reachable while they use its trees.
func NewView(db *memiavl.DB, logger log.Logger) *View {
	return newView(db, func() {
		if err := db.Close(); err != nil {
			logger.Error("failed to release mounted view", "err", err)
		}
	})
}

func newView(db *memiavl.DB, release func()) *View {
	v := &View{trees: db.Trees()}
	runtime.AddCleanup(v, func(release func()) { release() }, release)
	return v
}

// Mount holds the view every mounted store reads, so one Publish moves them all.
type Mount struct {
	view atomic.Pointer[View]
}

// Publish swaps in v, nil for none; readers still using the previous view keep it.
func (m *Mount) Publish(v *View) {
	m.view.Store(v)
}

// emptyTree serves a mounted store with no view, or no tree in it.
var emptyTree = memiavl.New(0)

// load returns the tree to read and the view to keep reachable while reading it.
func (st *Store) load() (*memiavl.Tree, *View) {
	if st.mount == nil {
		return st.tree, nil
	}
	v := st.mount.view.Load()
	if v == nil {
		return emptyTree, nil
	}
	if i := int(st.pos.Load()); i < len(v.trees) && v.trees[i].Name == st.name {
		return v.trees[i].Tree, v
	}
	return st.find(v), v
}

// find searches v by name, for a first read or after the stores in the db changed.
func (st *Store) find(v *View) *memiavl.Tree {
	i := slices.IndexFunc(v.trees, func(entry memiavl.NamedTree) bool { return entry.Name == st.name })
	if i < 0 {
		return emptyTree
	}
	st.pos.Store(int64(i))
	return v.trees[i].Tree
}

// viewIterator keeps its view, and so the snapshot it reads, mapped while it is in use.
type viewIterator struct {
	types.Iterator
	view *View
}

func (st *Store) Commit() types.CommitID {
	panic("memiavl store is not supposed to be committed alone")
}

func (st *Store) LastCommitID() types.CommitID {
	tree, v := st.load()
	defer runtime.KeepAlive(v)
	hash := tree.RootHash()
	return types.CommitID{
		Version: tree.Version(),
		Hash:    hash,
	}
}

// SetPruning panics as pruning options should be provided at initialization
// since IAVl accepts pruning options directly.
func (st *Store) SetPruning(_ pruningtypes.PruningOptions) {
	panic("cannot set pruning options on an initialized IAVL store")
}

// GetPruning panics as pruning options should be provided at initialization
// since IAVl accepts pruning options directly.
func (st *Store) GetPruning() pruningtypes.PruningOptions {
	panic("cannot get pruning options on an initialized IAVL store")
}

// GetStoreType Implements Store.
func (st *Store) GetStoreType() types.StoreType {
	return types.StoreTypeIAVL
}

func (st *Store) CacheWrap() types.CacheWrap {
	return cachekv.NewStore(st)
}

// CacheWrapWithTrace implements the Store interface.
// tracekv was removed in store/v2; fall back to regular CacheWrap.
func (st *Store) CacheWrapWithTrace(_ io.Writer, _ interface{}) types.CacheWrap {
	return cachekv.NewStore(st)
}

// Set Implements types.KVStore.
// we assume Set is only called in `Commit`, so the written state is only visible after commit.
func (st *Store) Set(key, value []byte) {
	st.changeSet.Pairs = append(st.changeSet.Pairs, &memiavl.KVPair{
		Key: key, Value: value,
	})
}

// Get Implements types.KVStore.
func (st *Store) Get(key []byte) []byte {
	tree, v := st.load()
	defer runtime.KeepAlive(v)
	return tree.Get(key)
}

// Has Implements types.KVStore.
func (st *Store) Has(key []byte) bool {
	tree, v := st.load()
	defer runtime.KeepAlive(v)
	return tree.Has(key)
}

// Delete Implements types.KVStore.
// we assume Delete is only called in `Commit`, so the written state is only visible after commit.
func (st *Store) Delete(key []byte) {
	st.changeSet.Pairs = append(st.changeSet.Pairs, &memiavl.KVPair{
		Key: key, Delete: true,
	})
}

func (st *Store) Iterator(start, end []byte) types.Iterator {
	return st.iterator(start, end, true)
}

func (st *Store) ReverseIterator(start, end []byte) types.Iterator {
	return st.iterator(start, end, false)
}

func (st *Store) iterator(start, end []byte, ascending bool) types.Iterator {
	tree, v := st.load()
	it := tree.Iterator(start, end, ascending)
	if v == nil {
		return it
	}
	return viewIterator{Iterator: it, view: v}
}

// SetInitialVersion sets the initial version of the IAVL tree. It is used when
// starting a new chain at an arbitrary height.
// implements interface StoreWithInitialVersion
func (st *Store) SetInitialVersion(version int64) {
	panic("memiavl store's SetInitialVersion is not supposed to be called directly")
}

// PopChangeSet returns the change set and clear it
func (st *Store) PopChangeSet() memiavl.ChangeSet {
	cs := st.changeSet
	st.changeSet = memiavl.ChangeSet{}
	return cs
}

func (st *Store) Query(req *types.RequestQuery) (res *types.ResponseQuery, err error) {
	if len(req.Data) == 0 {
		return nil, errors.Wrap(types.ErrTxDecode, "query cannot be zero length")
	}

	tree, v := st.load()
	defer runtime.KeepAlive(v)
	if req.Height > 0 && req.Height != tree.Version() {
		return nil, errors.Wrap(sdkerrors.ErrInvalidHeight, "invalid height")
	}

	res = &types.ResponseQuery{
		Height: tree.Version(),
	}

	switch req.Path {
	case "/key": // get by key
		res.Key = req.Data // data holds the key bytes
		res.Value = tree.Get(res.Key)

		if !req.Prove {
			break
		}

		// get proof from tree and convert to merkle.Proof before adding to result
		res.ProofOps, err = getProofFromTree(tree, req.Data, res.Value != nil)
		if err != nil {
			return nil, err
		}
	case "/subspace":
		pairs := memiavl.Pairs{
			Pairs: make([]memiavl.Pair, 0),
		}

		subspace := req.Data
		res.Key = subspace

		// the tree loaded above, so a concurrent swap can't change the version mid-query.
		iterator := tree.Iterator(subspace, types.PrefixEndBytes(subspace), true)
		for ; iterator.Valid(); iterator.Next() {
			pairs.Pairs = append(pairs.Pairs, memiavl.Pair{Key: iterator.Key(), Value: iterator.Value()})
		}
		err := iterator.Close()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to close iterator")
		}

		bz, err := pairs.Marshal()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal KV pairs")
		}

		res.Value = bz
	default:
		return nil, errors.Wrapf(sdkerrors.ErrUnknownRequest, "unexpected query path: %v", req.Path)
	}

	return res, nil
}

func (st *Store) WorkingHash() []byte {
	tree, v := st.load()
	defer runtime.KeepAlive(v)
	return tree.RootHash()
}

func getProofFromTree(tree *memiavl.Tree, key []byte, exists bool) (*cmtprotocrypto.ProofOps, error) {
	var (
		commitmentProof *ics23.CommitmentProof
		err             error
	)

	if exists {
		commitmentProof, err = tree.GetMembershipProof(key)
		if err != nil {
			return nil, errors.Wrapf(sdkerrors.ErrLogic, "failed to build membership proof for key %X: %s", key, err)
		}
	} else {
		commitmentProof, err = tree.GetNonMembershipProof(key)
		if err != nil {
			return nil, errors.Wrapf(sdkerrors.ErrInvalidRequest, "failed to build non-membership proof for key %X: %s", key, err)
		}
	}

	op := types.NewIavlCommitmentOp(key, commitmentProof)
	return &cmtprotocrypto.ProofOps{Ops: []cmtprotocrypto.ProofOp{op.ProofOp()}}, nil
}
