package x402

import (
	"context"
	"testing"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	ctypes "github.com/gnolang/gno/tm2/pkg/bft/rpc/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfirmationOf pins the decision the chain seam makes, which is the half of
// ConfirmTx that is not an RPC call. Every branch of it decides whether a payment
// is served, so none may rest on an untested assumption about the node's answer.
func TestConfirmationOf(t *testing.T) {
	want := []byte("0123456789abcdef0123456789abcdef")

	t.Run("a delivered transaction", func(t *testing.T) {
		state, err := confirmationOf(want, &ctypes.ResultTx{Hash: want})
		require.NoError(t, err)
		assert.Equal(t, Delivered, state)
	})

	t.Run("a refused delivery", func(t *testing.T) {
		state, err := confirmationOf(want, &ctypes.ResultTx{
			Hash:     want,
			TxResult: abci.ResponseDeliverTx{ResponseBase: abci.ResponseBase{Error: abci.StringError("refused")}},
		})
		require.NoError(t, err)
		assert.Equal(t, DeliveryFailed, state)
	})

	t.Run("no result at all", func(t *testing.T) {
		_, err := confirmationOf(want, nil)
		assert.Error(t, err, "an absent result is not a verdict on the payment")
	})

	// The node is asked for one hash and answers with a result carrying its own.
	// A result for some other transaction decides nothing about this payment, and
	// reading it as one would serve on a stranger's delivered send.
	t.Run("a result for another transaction", func(t *testing.T) {
		_, err := confirmationOf(want, &ctypes.ResultTx{Hash: []byte("some other transaction entirely")})
		assert.Error(t, err)
	})
}

// TestGnoclientNode_NodeChainIDWithoutAClient pins the same guard on the chain-id
// query an operator's startup check runs.
func TestGnoclientNode_NodeChainIDWithoutAClient(t *testing.T) {
	_, err := NewGnoclientNode(&gnoclient.Client{}).NodeChainID(context.Background())
	require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
}

// TestGnoclientNode_ConfirmTxWithoutAClient pins the guard that keeps a missing
// RPC client from panicking inside a payment path. gnoclient makes this check for
// its own methods; ConfirmTx reaches the RPC client directly, so it has to make it
// itself.
func TestGnoclientNode_ConfirmTxWithoutAClient(t *testing.T) {
	state, err := NewGnoclientNode(&gnoclient.Client{}).ConfirmTx(context.Background(), []byte("hash"))
	require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
	assert.Equal(t, NotCommitted, state, "a lookup that could not run reports no confirmation")
}
