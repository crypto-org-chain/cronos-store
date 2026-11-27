package memiavlstore

import (
	"testing"

	cmtprotocrypto "github.com/cometbft/cometbft/proto/tendermint/crypto"
	ics23 "github.com/cosmos/ics23/go"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/stretchr/testify/require"

	log "cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/store/v2/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func TestQueryProve(t *testing.T) {
	present, absent := []byte("b"), []byte("c")

	query := func(t *testing.T, st *Store, key []byte) (*cmtprotocrypto.ProofOps, error) {
		res, err := st.Query(&types.RequestQuery{Path: "/key", Data: key, Prove: true})
		if err != nil {
			require.Nil(t, res)
			return nil, err
		}
		return res.ProofOps, nil
	}
	// Query can't reach these: exists disagrees with the tree only if it changed after the Get.
	proveAs := func(exists bool) func(*testing.T, *Store, []byte) (*cmtprotocrypto.ProofOps, error) {
		return func(_ *testing.T, st *Store, key []byte) (*cmtprotocrypto.ProofOps, error) {
			return getProofFromTree(st.tree, key, exists)
		}
	}

	testCases := []struct {
		name   string
		empty  bool
		key    []byte
		prove  func(*testing.T, *Store, []byte) (*cmtprotocrypto.ProofOps, error)
		expErr error
	}{
		{name: "absent key, empty tree", empty: true, key: absent, prove: query, expErr: sdkerrors.ErrInvalidRequest},
		{name: "absent key", key: absent, prove: query},
		{name: "present key", key: present, prove: query},
		{name: "membership of an absent key", key: absent, prove: proveAs(true), expErr: sdkerrors.ErrLogic},
		{name: "non-membership of a present key", key: present, prove: proveAs(false), expErr: sdkerrors.ErrLogic},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tree := memiavl.NewEmptyTree(0, 0)
			if !tc.empty {
				tree.ApplyChangeSet(memiavl.ChangeSet{Pairs: []*memiavl.KVPair{
					{Key: []byte("a"), Value: []byte("1")},
					{Key: present, Value: []byte("2")},
					{Key: []byte("d"), Value: []byte("3")},
				}})
			}
			_, _, err := tree.SaveVersion(true)
			require.NoError(t, err)

			ops, err := tc.prove(t, New(tree, log.NewNopLogger()), tc.key)
			if tc.expErr != nil {
				require.ErrorIs(t, err, tc.expErr)
				require.Nil(t, ops)
				return
			}
			require.NoError(t, err)
			require.Len(t, ops.Ops, 1)
			require.Equal(t, types.ProofOpIAVLCommitment, ops.Ops[0].Type)
			require.Equal(t, tc.key, ops.Ops[0].Key)

			var proof ics23.CommitmentProof
			require.NoError(t, proof.Unmarshal(ops.Ops[0].Data))
			if value := tree.Get(tc.key); value != nil {
				require.True(t, ics23.VerifyMembership(ics23.IavlSpec, tree.RootHash(), &proof, tc.key, value))
			} else {
				require.True(t, ics23.VerifyNonMembership(ics23.IavlSpec, tree.RootHash(), &proof, tc.key))
			}
		})
	}
}
