package facilitator

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/ed25519"
	tm2errors "github.com/gnolang/gno/tm2/pkg/errors"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountChainID is the chain the signed fixtures below are signed for. The
// sign doc covers it, so verifying against another chain-id must fail.
const accountChainID = "dev"

// signedPayment describes the payment the fixtures price, signed for real
// against the sign-doc inputs the auth ante would use. Account checks exercise
// crypto, so txFixture's placeholder signature proves nothing here.
type signedPayment struct {
	key         crypto.PrivKey
	chainID     string
	number      uint64
	sequence    uint64
	sessionAddr crypto.Address // zero for a master-key signature
	from        crypto.Address // zero defaults to the signing key's own address
}

func (p signedPayment) tx(t *testing.T) *std.Tx {
	t.Helper()
	from := p.from
	if from.IsZero() {
		from = p.key.PubKey().Address()
	}
	req := reqFixture()
	tx := &std.Tx{
		Msgs: []std.Msg{bank.MsgSend{
			FromAddress: from,
			ToAddress:   crypto.MustAddressFromString(req.PayTo),
			Amount:      std.MustParseCoins(req.Amount + req.Asset),
		}},
		Fee: std.NewFee(100000, std.MustParseCoin("1000000ugnot")),
	}
	signBytes, err := tx.GetSignBytes(p.chainID, p.number, p.sequence)
	require.NoError(t, err)
	signature, err := p.key.Sign(signBytes)
	require.NoError(t, err)
	tx.Signatures = []std.Signature{{
		PubKey:      p.key.PubKey(),
		Signature:   signature,
		SessionAddr: p.sessionAddr,
	}}
	return tx
}

func masterKey(t *testing.T) crypto.PrivKey {
	t.Helper()
	return ed25519.GenPrivKeyFromSecret([]byte("x402-account-master"))
}

// accountAt is the on-chain account a signature keyed by key verifies against
// at the given account number and sequence.
func accountAt(key crypto.PrivKey, number, sequence uint64) SignerAccount {
	return accountWithKey(key.PubKey(), number, sequence)
}

// accountWithKey is the same for an explicit stored key, including none — the shape
// of an account that has never signed.
func accountWithKey(pub crypto.PubKey, number, sequence uint64) SignerAccount {
	return SignerAccount{Account: &std.BaseAccount{
		PubKey:        pub,
		AccountNumber: number,
		Sequence:      sequence,
	}}
}

// TestVerifySignature_NoAccountVerifiesNothing pins the fail-closed answer for an
// AccountReader that returned no account at all. AccountReader is exported, so this
// is a contract a third-party implementation can break — inside a payment path,
// where a panic would be the worst available answer.
func TestVerifySignature_NoAccountVerifiesNothing(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)

	state := verifySignature(tx, SignerAccount{}, key.PubKey().Address(), accountChainID)
	assert.Equal(t, signatureUnverifiable, state)
}

// accountReason is the reason the account checks refuse tx, requiring the
// verdict to agree with it: a reason is reported only alongside a refusal, and a
// payment the account accepts carries none. The cases that carry a cause assert
// it directly, so it is dropped here.
func accountReason(t *testing.T, tx *std.Tx, node *fakeNode) string {
	t.Helper()
	verdict, reason, _ := verifyAccountChecks(context.Background(), tx, node, accountChainID)
	if reason == "" {
		require.Equal(t, paymentAccepted, verdict)
	} else {
		require.Equal(t, paymentRefused, verdict)
	}
	return reason
}

// TestVerifyAccountChecks_SignedPaymentSatisfiesStaticVerification pins the
// precondition the account checks rely on: the signed fixtures are payments
// VerifyStatic accepts, so exactly one message and one signature are present
// and indexing them is sound.
func TestVerifyAccountChecks_SignedPaymentSatisfiesStaticVerification(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)

	raw, err := amino.Marshal(*tx)
	require.NoError(t, err)
	decoded, payer, reason := VerifyStatic(reqFixture(), SchemePayload{
		Transaction: base64.StdEncoding.EncodeToString(raw),
	})
	require.Empty(t, reason)
	require.NotNil(t, decoded)
	assert.Equal(t, key.PubKey().Address().String(), payer)
}

func TestVerifyAccountChecks_MasterSignedAtCurrentSequence(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)

	assert.Empty(t, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 3)}))
}

// TestVerifyAccountChecks_SessionSignedUsesTheSessionAccountsOwnNumbers pins the
// costliest mistake in account resolution. A session-signed transaction's sign
// bytes cover the SESSION account's account number and sequence, not the
// master's, so a verifier reading the master's numbers refuses every delegated
// payment the chain would settle.
func TestVerifyAccountChecks_SessionSignedUsesTheSessionAccountsOwnNumbers(t *testing.T) {
	const (
		sessionNumber, sessionSequence = 41, 2
		masterNumber, masterSequence   = 7, 19
	)
	sessionKey := ed25519.GenPrivKeyFromSecret([]byte("x402-account-session"))
	tx := signedPayment{
		key:         sessionKey,
		chainID:     accountChainID,
		number:      sessionNumber,
		sequence:    sessionSequence,
		sessionAddr: sessionKey.PubKey().Address(),
		from:        masterKey(t).PubKey().Address(),
	}.tx(t)

	t.Run("verified against the session account", func(t *testing.T) {
		node := &fakeNode{account: accountAt(sessionKey, sessionNumber, sessionSequence)}
		assert.Empty(t, accountReason(t, tx, node))
	})

	t.Run("verified against the master account", func(t *testing.T) {
		node := &fakeNode{account: accountAt(sessionKey, masterNumber, masterSequence)}
		assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, node),
			"the master's numbers do not cover a session-signed payment")
	})
}

// TestVerifyAccountChecks_SignatureOverAConsumedSequence separates a stale
// payment from a forged one. A signature that verifies one sequence back is a
// valid signature over a sequence the account has already spent, which is proof
// of staleness rather than a guess.
func TestVerifyAccountChecks_SignatureOverAConsumedSequence(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)

	t.Run("one sequence back is reported stale", func(t *testing.T) {
		assert.Equal(t, ReasonSequenceMismatch, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 4)}))
	})

	// The residual of probing a single sequence back: a payment superseded more
	// than once is indistinguishable from a forgery to this check.
	t.Run("two sequences back is reported invalid", func(t *testing.T) {
		assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 5)}))
	})
}

func TestVerifyAccountChecks_TamperedSignature(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)
	tx.Signatures[0].Signature[0] ^= 0xff

	assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 3)}))
}

// TestVerifyAccountChecks_SignedForAnotherChain pins that the chain-id inside
// the sign doc is checked. A payment signed for another chain replays onto this
// one unless the signature is verified against this chain's id.
func TestVerifyAccountChecks_SignedForAnotherChain(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: "other-chain", number: 7, sequence: 3}.tx(t)

	assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 3)}))
}

func TestVerifyAccountChecks_SignedForAnotherAccountNumber(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)

	assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, &fakeNode{account: accountAt(key, 8, 3)}))
}

// TestVerifyAccountChecks_ChainAccessFailureIsNotAVerdict pins the distinction
// the account read rests on. One call reports both an account the chain says does
// not exist — a payment verdict — and a read that never reached the chain, which
// decides nothing about the payment.
//
// They are told apart by type, never by prose: gno answers every refusal it
// decides with an abci.Error, the absent account among them, while a dial
// failure, a timeout and an encoding change carry none. tm2's JSON-RPC layer
// flattens its own failures into a single code whose prose is the only
// difference, so no verdict here may rest on an error string.
func TestVerifyAccountChecks_ChainAccessFailureIsNotAVerdict(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)

	t.Run("the chain holds no account for the signer", func(t *testing.T) {
		// What gnoclient.QueryAccount answers a null account response with. The
		// auth ante resolves the signer's account before it verifies anything, so
		// this transaction is one neither simulation nor delivery would accept.
		node := &fakeNode{accountErr: std.ErrUnknownAddress("unknown address: " + key.PubKey().Address().String())}
		verdict, reason, cause := verifyAccountChecks(context.Background(), tx, node, accountChainID)

		assert.Equal(t, paymentRefused, verdict)
		assert.Equal(t, ReasonSimulationFailed, reason, "the refusal simulating would have reported, one read earlier")
		// That reason covers more than one refusal, so the cause is what tells an
		// operator which of them refused.
		assert.ErrorContains(t, cause, "unknown address")
	})

	t.Run("the account could not be read at all", func(t *testing.T) {
		// The shape gnoclient.QueryAccount wraps a failed ABCI query in.
		node := &fakeNode{accountErr: tm2errors.Wrap(errors.New("dial tcp 127.0.0.1:26657: connection refused"), "query account")}
		verdict, reason, cause := verifyAccountChecks(context.Background(), tx, node, accountChainID)

		assert.Equal(t, noVerdict, verdict)
		assert.Empty(t, reason, "a party whose own chain access failed holds no reason to report")
		assert.ErrorContains(t, cause, "connection refused")
	})
}

// TestVerifyAccountChecks_SequenceZeroDoesNotProbeBelowZero pins the guard on
// the consumed-sequence probe. Unguarded, an account at sequence 0 probes
// 2^64-1, and a signature over that sequence would be reported merely stale.
func TestVerifyAccountChecks_SequenceZeroDoesNotProbeBelowZero(t *testing.T) {
	key := masterKey(t)

	t.Run("the first payment from a fresh account is accepted", func(t *testing.T) {
		tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 0}.tx(t)
		assert.Empty(t, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 0)}))
	})

	t.Run("a signature over the wrapped sequence is invalid, not stale", func(t *testing.T) {
		tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: math.MaxUint64}.tx(t)
		assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 0)}))
	})
}

// TestVerifyAccountChecks_ResolvesTheKeyTheAnteWouldUse covers the four ways the
// ante picks the key a signature is verified against. Reading only the stored
// key refuses every payer whose first transaction is this payment; reading only
// the signature's key accepts a key that was never the account's.
func TestVerifyAccountChecks_ResolvesTheKeyTheAnteWouldUse(t *testing.T) {
	key := masterKey(t)
	other := ed25519.GenPrivKeyFromSecret([]byte("x402-account-other"))

	t.Run("a signature omitting its key uses the stored one", func(t *testing.T) {
		tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)
		tx.Signatures[0].PubKey = nil
		assert.Empty(t, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 3)}))
	})

	t.Run("a stored key disagreeing with the signature's is refused", func(t *testing.T) {
		tx := signedPayment{key: other, chainID: accountChainID, number: 7, sequence: 3, from: key.PubKey().Address()}.tx(t)
		assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, &fakeNode{account: accountAt(key, 7, 3)}))
	})

	t.Run("an account storing no key adopts the signature's own", func(t *testing.T) {
		tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 0}.tx(t)
		node := &fakeNode{account: accountWithKey(nil, 7, 0)}
		assert.Empty(t, accountReason(t, tx, node))
	})

	t.Run("an account storing no key refuses a key for another address", func(t *testing.T) {
		tx := signedPayment{key: other, chainID: accountChainID, number: 7, sequence: 0, from: key.PubKey().Address()}.tx(t)
		node := &fakeNode{account: accountWithKey(nil, 7, 0)}
		assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, node))
	})

	t.Run("neither the account nor the signature names a key", func(t *testing.T) {
		tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 0}.tx(t)
		tx.Signatures[0].PubKey = nil
		node := &fakeNode{account: accountWithKey(nil, 7, 0)}
		assert.Equal(t, ReasonSignatureInvalid, accountReason(t, tx, node))
	})
}

// TestVerifyAccountChecks_RefusesAnAmbiguousSignerSet restates the precondition
// as behavior: this check indexes the single signature and the single signer, so
// a transaction carrying another count must be refused rather than indexed.
func TestVerifyAccountChecks_RefusesAnAmbiguousSignerSet(t *testing.T) {
	key := masterKey(t)
	tx := signedPayment{key: key, chainID: accountChainID, number: 7, sequence: 3}.tx(t)
	tx.Signatures = nil

	verdict, reason, cause := verifyAccountChecks(context.Background(), tx, &fakeNode{account: accountAt(key, 7, 3)}, accountChainID)
	assert.Equal(t, paymentRefused, verdict)
	assert.Equal(t, ReasonSignatureCount, reason)
	assert.ErrorContains(t, cause, "exactly one signer and one signature")
}
