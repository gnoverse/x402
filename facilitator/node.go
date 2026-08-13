package facilitator

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// NewGnoclientNode adapts a gnoclient.Client to the chain access a facilitator
// settles through. It holds no mutable state beyond the client handle, so one
// instance is safe to share across concurrent handler goroutines. The client
// needs no signer: a facilitator does not sign, it relays transactions the payer
// already signed.
func NewGnoclientNode(cli *gnoclient.Client) *GnoclientNode {
	return &GnoclientNode{cli: cli}
}

// GnoclientNode is the gnoclient-backed chain access returned by
// NewGnoclientNode.
type GnoclientNode struct {
	cli *gnoclient.Client
}

var _ Node = (*GnoclientNode)(nil)

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
//
// The error is returned untouched, for the same reason Simulate's is: only the
// chain's own abci.Error makes it a verdict on the transaction, and chainRefused
// reads that off the type.
//
// A result travels with it when there is one. Upstream answers the CheckTx and
// DeliverTx paths with both, and a delivery the chain committed and then aborted
// charged the payer its fee — so that transaction's hash is a real record, and
// the only one an operator can reconcile the charge against. A transport failure
// carries no result and leaves nothing to report.
func (n *GnoclientNode) Broadcast(tx *std.Tx) (string, int64, error) {
	res, err := n.cli.BroadcastTxCommit(tx)
	if res == nil {
		return "", 0, err
	}
	return hex.EncodeToString(res.Hash), res.Height, err
}
