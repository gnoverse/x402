package x402

import (
	"context"
	"errors"

	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/sdk/auth"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// SignerAccount is the on-chain account a transaction's signature is verified
// against.
//
// It carries the account itself rather than a copy of the fields a sign doc needs,
// so the sign bytes come from upstream's own auth.GetSignBytes: which account
// fields enter the document is a decision the chain makes, and re-deriving it here
// would be a drift channel on the one check where a silent accept is catastrophic.
type SignerAccount struct {
	Account std.Account
}

// pubKey is the key the account stores, or nil for an account that has never
// signed — and for no account at all.
func (a SignerAccount) pubKey() crypto.PubKey {
	if a.Account == nil {
		return nil
	}
	return a.Account.GetPubKey()
}

// sequence is the account's current sequence — the one a fresh signature commits
// to.
func (a SignerAccount) sequence() uint64 {
	if a.Account == nil {
		return 0
	}
	return a.Account.GetSequence()
}

// number is the account number the sign doc covers.
func (a SignerAccount) number() uint64 {
	if a.Account == nil {
		return 0
	}
	return a.Account.GetAccountNumber()
}

// AccountReader resolves the account a transaction's signature is verified
// against.
type AccountReader interface {
	SignerAccount(ctx context.Context, tx *std.Tx) (SignerAccount, error)
}

// signatureState is what a signature proves about the account's sequence.
type signatureState int

const (
	signatureFresh        signatureState = iota // verifies at the account's current sequence
	signatureConsumed                           // verifies at the previous sequence
	signatureUnverifiable                       // verifies at neither
)

// txSigner returns the address whose account authorizes the transaction, the way
// the auth ante resolves it. For the bank.MsgSend a payment carries, that is
// FromAddress — the payer VerifyStatic reports.
//
// Precondition: exactly one message and exactly one signature, which VerifyStatic
// already enforces. It is restated as an error because the callers below index
// both slices.
func txSigner(tx *std.Tx) (crypto.Address, error) {
	signers := tx.GetSigners()
	if len(signers) != 1 || len(tx.Signatures) != 1 {
		return crypto.Address{}, errors.New("payment must carry exactly one signer and one signature")
	}
	return signers[0], nil
}

// verifySignature reports which sequence, if any, the transaction's signature
// verifies at. The sign doc covers the chain-id, the account number, the
// sequence, the fee, the messages and the memo — so this one check decides the
// signature, the chain and the account's freshness together.
//
// A std.Tx records no sequence, so a verifier must supply one, and a single
// failed verification cannot separate a forged signature from a stale one.
// Verifying at the previous sequence resolves that: a signature valid over a
// sequence the account has already consumed is proof of staleness. Only one
// sequence back is probed — a session consumes sequences one at a time, so a
// superseded payment is stale by exactly one, and a deeper probe would spend
// more verification per request to better label a rarer case. A payment stale by
// more than one sequence therefore reports as unverifiable.
func verifySignature(tx *std.Tx, acc SignerAccount, signer crypto.Address, chainID string) signatureState {
	// An AccountReader that answered with no account established nothing, and no
	// signature can be verified against it. AccountReader is exported, so this is
	// a contract a third-party implementation could break — inside a payment path,
	// where a panic is the worst available answer.
	if acc.Account == nil {
		return signatureUnverifiable
	}
	key := signingKey(tx.Signatures[0], acc, signer)
	if key == nil {
		return signatureUnverifiable
	}
	verifies := func(signBytes []byte, err error) bool {
		if err != nil {
			// A signing payload amino cannot produce is a payload no signature
			// covers, so no sequence verifies.
			return false
		}
		return key.VerifyBytes(signBytes, tx.Signatures[0].Signature)
	}
	// The fresh check is upstream's own: auth.GetSignBytes builds the document the
	// ante verifies against, from the same account, so the two cannot disagree
	// about which fields it covers. (The genesis flag is false — a facilitator
	// serves a running chain, and the genesis document is tx.GetSignBytes at
	// account 0, sequence 0.)
	if verifies(auth.GetSignBytes(chainID, *tx, acc.Account, false)) {
		return signatureFresh
	}
	// The staleness probe has no upstream equivalent, because no account holds the
	// previous sequence: it is asked explicitly, at the level below.
	if seq := acc.sequence(); seq > 0 && verifies(tx.GetSignBytes(chainID, acc.number(), seq-1)) {
		return signatureConsumed
	}
	return signatureUnverifiable
}

// signingKey returns the key gno's auth ante verifies this signature against, or
// nil when no key can satisfy it.
//
// A stored key is the only key the account accepts, and a signature naming a
// different one is refused before verification. An account storing none has never
// signed, so the ante adopts the signature's own key — but only for the address
// that key derives, because a master address is derived lazily and the first
// signer to claim an unseen one would otherwise fix its key. A session account
// stores the key it was created with, so this branch is reached for master
// accounts only.
func signingKey(sig std.Signature, acc SignerAccount, signer crypto.Address) crypto.PubKey {
	if stored := acc.pubKey(); stored != nil {
		if sig.PubKey != nil && !stored.Equals(sig.PubKey) {
			return nil
		}
		return stored
	}
	if sig.PubKey == nil || sig.PubKey.Address() != signer {
		return nil
	}
	return sig.PubKey
}
