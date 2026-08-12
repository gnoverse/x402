package facilitator

import (
	"context"
	"testing"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/stretchr/testify/require"
)

// TestGnoclientNode_NodeChainIDWithoutAClient pins the guard on the chain-id
// query an operator's startup check runs: a missing RPC client must refuse the
// lookup, not panic.
func TestGnoclientNode_NodeChainIDWithoutAClient(t *testing.T) {
	_, err := NewGnoclientNode(&gnoclient.Client{}).NodeChainID(context.Background())
	require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
}
