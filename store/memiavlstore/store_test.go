package memiavlstore

import (
	"testing"

	ics23 "github.com/cosmos/ics23/go"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/store/v2/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func TestGetProofFromTree(t *testing.T) {
	present, absent := []byte("b"), []byte("c")

	testCases := []struct {
		name   string
		empty  bool
		key    []byte
		exists bool
		expErr error
	}{
		{name: "absent key, empty tree", empty: true, key: absent, expErr: sdkerrors.ErrInvalidRequest},
		// exists disagrees with the tree only if it changed after the Get; never a client error.
		{name: "membership of an absent key", key: absent, exists: true, expErr: sdkerrors.ErrLogic},
		{name: "non-membership of a present key", key: present, expErr: sdkerrors.ErrLogic},
		{name: "absent key", key: absent},
		{name: "present key", key: present, exists: true},
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
				_, _, err := tree.SaveVersion(true)
				require.NoError(t, err)
			}

			res, err := getProofFromTree(tree, tc.key, tc.exists)
			if tc.expErr != nil {
				require.ErrorIs(t, err, tc.expErr)
				require.Nil(t, res)
				return
			}
			require.NoError(t, err)
			require.Len(t, res.Ops, 1)
			require.Equal(t, types.ProofOpIAVLCommitment, res.Ops[0].Type)
			require.Equal(t, tc.key, res.Ops[0].Key)

			var proof ics23.CommitmentProof
			require.NoError(t, proof.Unmarshal(res.Ops[0].Data))
			if tc.exists {
				require.True(t, ics23.VerifyMembership(ics23.IavlSpec, tree.RootHash(), &proof, tc.key, tree.Get(tc.key)))
			} else {
				require.True(t, ics23.VerifyNonMembership(ics23.IavlSpec, tree.RootHash(), &proof, tc.key))
			}
		})
	}
}
