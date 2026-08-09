package x402

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	ctypes "github.com/gnolang/gno/tm2/pkg/bft/rpc/core/types"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// NewGnoclientNode adapts a gnoclient.Client to the chain access this package
// needs: the Node a facilitator settles through, and the Confirmer a seller
// reads. It holds no mutable state beyond the client handle, so one instance is
// safe to share across concurrent handler goroutines — the same pattern
// internal/chain.Real uses for its *gnoclient.Client. The client needs no
// signer: neither role signs, they relay and read already-signed transactions.
//
// The concrete type is returned so one adapter can serve both interfaces; which
// powers a caller receives is decided by the field it assigns this to, and a
// seller's Confirmer cannot broadcast.
func NewGnoclientNode(cli *gnoclient.Client) *GnoclientNode {
	return &GnoclientNode{cli: cli}
}

// GnoclientNode is the gnoclient-backed chain access returned by
// NewGnoclientNode.
type GnoclientNode struct {
	cli *gnoclient.Client
}

var (
	_ Node      = (*GnoclientNode)(nil)
	_ Confirmer = (*GnoclientNode)(nil)
)

// SignerAccount resolves the account gno's auth ante verifies this
// transaction's signature against: the session account the signature names when
// it carries one, the signer's own account otherwise. Both queries are
// upstream's, so the ABCI paths and the amino-JSON decoding stay in one place.
//
// Both failures are wrapped with %w rather than described, because the wrapped
// error is what separates a payment verdict from this facilitator's own outage:
// upstream answers an account the chain holds none of with
// std.ErrUnknownAddress, and a query that never reached the node with the
// transport error. chainRefused reads that difference off the type.
//
// The context states the contract for a network call but cannot be honored:
// gnoclient's QueryAccount and QuerySessionAccount both pass
// context.Background() to the RPC client, so a cancelled context only takes
// effect between calls. Threading it would mean forking upstream's query and
// decode logic, which is what this adapter exists to reuse.
func (n *GnoclientNode) SignerAccount(_ context.Context, tx *std.Tx) (SignerAccount, error) {
	signer, err := txSigner(tx)
	if err != nil {
		return SignerAccount{}, err
	}
	if sessionAddr := tx.Signatures[0].SessionAddr; !sessionAddr.IsZero() {
		acc, _, err := n.cli.QuerySessionAccount(signer, sessionAddr)
		if err != nil {
			return SignerAccount{}, fmt.Errorf("query session account: %w", err)
		}
		return signerAccountOf(acc), nil
	}
	acc, _, err := n.cli.QueryAccount(signer)
	if err != nil {
		return SignerAccount{}, fmt.Errorf("query account: %w", err)
	}
	return signerAccountOf(acc), nil
}

// signerAccountOf wraps an on-chain account as the account a signature is verified
// against. Both a master account and a session account satisfy std.Account, so one
// reader covers both, and the account travels whole — the sign doc is built from it
// by upstream rather than from fields copied out of it.
func signerAccountOf(acc std.Account) SignerAccount {
	return SignerAccount{Account: acc}
}

// Simulate reports the simulated delivery as an error: gnoclient.Simulate
// already treats a failed ResponseDeliverTx as an error return, so no
// separate result inspection is needed here.
//
// That error is returned untouched, because it is what tells a refused delivery
// apart from a node that never answered: upstream wraps the response's own
// abci.Error for the first — the node reports every refusal it decides as one —
// and the query, marshalling or decoding failure for the second. chainRefused
// reads that difference off the type, so wrapping it in prose here would collapse
// the two back together.
func (n *GnoclientNode) Simulate(tx *std.Tx) error {
	_, err := n.cli.Simulate(tx)
	return err
}

// ConfirmTx reads the chain's own record for a transaction hash. Unlike the
// account queries above, this one does honor the context: RPCClient.Tx takes it
// and passes it down.
//
// Every failure reports as an error rather than as NotCommitted, including a
// hash the chain holds no result for. tm2 offers no way to separate them: the
// node answers a missing result with state.NoTxResultForHashError, but the
// JSON-RPC layer flattens every handler error into one internal-error code whose
// prose is the only difference, and a delivery decision must not rest on an
// upstream error string. Neither answer may be served on, so the distinction
// buys nothing.
func (n *GnoclientNode) ConfirmTx(ctx context.Context, hash []byte) (Confirmation, error) {
	// The RPC client is reached directly rather than through a gnoclient
	// method, so the check gnoclient makes for its own calls is made here: a
	// missing client must refuse the lookup, not panic inside a payment path.
	if n.cli.RPCClient == nil {
		return NotCommitted, gnoclient.ErrMissingRPCClient
	}
	res, err := n.cli.RPCClient.Tx(ctx, hash)
	if err != nil {
		return NotCommitted, fmt.Errorf("query transaction: %w", err)
	}
	return confirmationOf(hash, res)
}

// confirmationOf reads a lookup result as a confirmation for the hash that was
// asked about.
//
// The hash is re-checked against the answer because the whole point of deriving it
// was to bind the lookup to the payment: a result carrying some other
// transaction's hash decides nothing about this one, and taking it as a verdict
// would serve on a stranger's delivered send. An honest node echoes what it was
// asked; nothing about serving a payment should depend on that.
func confirmationOf(want []byte, res *ctypes.ResultTx) (Confirmation, error) {
	switch {
	case res == nil:
		return NotCommitted, errors.New("query transaction: the node returned no result")
	case !bytes.Equal(res.Hash, want):
		return NotCommitted, fmt.Errorf("query transaction: the node answered for %x, not %x", res.Hash, want)
	case res.TxResult.IsErr():
		return DeliveryFailed, nil
	default:
		return Delivered, nil
	}
}

// NodeChainID is the chain id the node reports for itself.
//
// A facilitator or seller is configured with a chain-id and an RPC endpoint
// separately, and every signature is verified against the sign doc for that
// chain-id while transactions go wherever the endpoint points. Nothing in a
// payment can detect the disagreement: a mismatch refuses every payment as
// signature_invalid, blaming payers for an operator's error. Asking the node at
// startup removes the class.
func (n *GnoclientNode) NodeChainID(ctx context.Context) (string, error) {
	if n.cli.RPCClient == nil {
		return "", gnoclient.ErrMissingRPCClient
	}
	status, err := n.cli.RPCClient.Status(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("query node status: %w", err)
	}
	if status == nil {
		return "", errors.New("query node status: the node returned no result")
	}
	return status.NodeInfo.Network, nil
}

// Broadcast reports a failed CheckTx or DeliverTx as an error:
// gnoclient.BroadcastTxCommit already wraps both into its returned error, so
// no separate result inspection is needed here.
func (n *GnoclientNode) Broadcast(tx *std.Tx) (string, int64, error) {
	res, err := n.cli.BroadcastTxCommit(tx)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(res.Hash), res.Height, nil
}
