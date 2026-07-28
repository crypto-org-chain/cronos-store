package memiavlstore

import (
	"io"
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
	// SetTree runs from rootmulti.Store.publishQuerySnapshot while queries on this
	// Store may run on another ABCI connection, so the swap has to be atomic. The
	// tree it publishes is a Copy(), never the live tree rootmulti's flush mutates
	// in place, so readers holding the old pointer stay on stable nodes.
	tree   atomic.Pointer[memiavl.Tree]
	logger log.Logger

	changeSet memiavl.ChangeSet
}

func New(tree *memiavl.Tree, logger log.Logger) *Store {
	st := &Store{logger: logger}
	st.tree.Store(tree)
	return st
}

func (st *Store) SetTree(tree *memiavl.Tree) {
	st.tree.Store(tree)
}

func (st *Store) Commit() types.CommitID {
	panic("memiavl store is not supposed to be committed alone")
}

func (st *Store) LastCommitID() types.CommitID {
	tree := st.tree.Load()
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
	return st.tree.Load().Get(key)
}

// Has Implements types.KVStore.
func (st *Store) Has(key []byte) bool {
	return st.tree.Load().Has(key)
}

// Delete Implements types.KVStore.
// we assume Delete is only called in `Commit`, so the written state is only visible after commit.
func (st *Store) Delete(key []byte) {
	st.changeSet.Pairs = append(st.changeSet.Pairs, &memiavl.KVPair{
		Key: key, Delete: true,
	})
}

func (st *Store) Iterator(start, end []byte) types.Iterator {
	return st.tree.Load().Iterator(start, end, true)
}

func (st *Store) ReverseIterator(start, end []byte) types.Iterator {
	return st.tree.Load().Iterator(start, end, false)
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

	tree := st.tree.Load()
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

		// iterate the tree loaded above, not st: a concurrent SetTree would
		// otherwise return pairs from a newer version than res.Height reports.
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
	return st.tree.Load().RootHash()
}

// getProofFromTree builds the merkle proof for key, either an existence or an
// absence proof depending on `exists`. An empty tree can produce neither, so the
// error is returned to the querier instead of taking the node down.
func getProofFromTree(tree *memiavl.Tree, key []byte, exists bool) (*cmtprotocrypto.ProofOps, error) {
	var (
		commitmentProof *ics23.CommitmentProof
		err             error
	)

	if exists {
		commitmentProof, err = tree.GetMembershipProof(key)
	} else {
		commitmentProof, err = tree.GetNonMembershipProof(key)
	}
	if err != nil {
		return nil, errors.Wrapf(sdkerrors.ErrInvalidRequest, "failed to build proof for key %X: %s", key, err)
	}

	op := types.NewIavlCommitmentOp(key, commitmentProof)
	return &cmtprotocrypto.ProofOps{Ops: []cmtprotocrypto.ProofOp{op.ProofOp()}}, nil
}
