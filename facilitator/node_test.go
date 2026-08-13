package facilitator

import (
	"context"
	"errors"
	"testing"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	ctypes "github.com/gnolang/gno/tm2/pkg/bft/rpc/core/types"
	bfttypes "github.com/gnolang/gno/tm2/pkg/bft/types"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGnoclientNode_NodeChainIDWithoutAClient pins the guard on the chain-id
// query an operator's startup check runs: a missing RPC client must refuse the
// lookup, not panic.
func TestGnoclientNode_NodeChainIDWithoutAClient(t *testing.T) {
	_, err := NewGnoclientNode(&gnoclient.Client{}).NodeChainID(context.Background())
	require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
}

// stubRPCClient answers one broadcast and nothing else. The transport interface
// is a composite of seven, so it is embedded to supply the rest: any other call
// is a nil-interface panic naming the method the test did not expect.
type stubRPCClient struct {
	rpcclient.Client
	broadcast func() (*ctypes.ResultBroadcastTxCommit, error)
}

func (s stubRPCClient) BroadcastTxCommit(context.Context, bfttypes.Tx) (*ctypes.ResultBroadcastTxCommit, error) {
	return s.broadcast()
}

func broadcastThrough(t *testing.T, res *ctypes.ResultBroadcastTxCommit, err error) (string, int64, error) {
	t.Helper()
	node := NewGnoclientNode(&gnoclient.Client{RPCClient: stubRPCClient{
		broadcast: func() (*ctypes.ResultBroadcastTxCommit, error) { return res, err },
	}})
	return node.Broadcast(&std.Tx{})
}

// TestGnoclientNode_BroadcastReportsTheTxTheChainAborted keeps the transaction
// hash of a delivery the chain committed and then refused. Such a transaction
// exists on chain and charged the payer its fee, so the hash is the one record
// an operator can reconcile against — and upstream returns the result alongside
// the error precisely for the CheckTx and DeliverTx paths.
func TestGnoclientNode_BroadcastReportsTheTxTheChainAborted(t *testing.T) {
	hash, height, err := broadcastThrough(t, &ctypes.ResultBroadcastTxCommit{
		DeliverTx: abci.ResponseDeliverTx{
			ResponseBase: abci.ResponseBase{Error: std.SessionExpiredError{}},
		},
		Hash:   []byte{0xde, 0xad, 0xbe, 0xef},
		Height: 12,
	}, nil)

	require.Error(t, err)
	assert.True(t, chainRefused(err), "an aborted delivery is the chain's own answer")
	assert.Equal(t, "deadbeef", hash)
	assert.Equal(t, int64(12), height)
}

// TestGnoclientNode_BroadcastReportsNoTxWhenTheChainNeverAnswered pins the other
// half: a transport failure carries no result, so there is no hash to report and
// nothing about the transaction is known.
func TestGnoclientNode_BroadcastReportsNoTxWhenTheChainNeverAnswered(t *testing.T) {
	hash, height, err := broadcastThrough(t, nil, errors.New("connection refused"))

	require.Error(t, err)
	assert.False(t, chainRefused(err), "an unreachable node refuses nothing")
	assert.Empty(t, hash)
	assert.Zero(t, height)
}
