package rootmulti

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"cosmossdk.io/store/types"
)

const testChainId = "test_chain"

func TestLastCommitID(t *testing.T) {
	store := NewStore(t.TempDir(), log.NewNopLogger(), false, false, testChainId)
	require.Equal(t, types.CommitID{}, store.LastCommitID())
}
