package rootmulti

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"cosmossdk.io/store/types"
)

const chainId = "test_chain"

func TestLastCommitID(t *testing.T) {
	store := NewStore(t.TempDir(), log.NewNopLogger(), false, false, chainId)
	require.Equal(t, types.CommitID{}, store.LastCommitID())
}
