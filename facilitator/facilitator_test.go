package facilitator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/ed25519"
	"github.com/gnolang/gno/tm2/pkg/crypto/multisig"
	"github.com/gnolang/gno/tm2/pkg/crypto/multisig/bitarray"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNode struct {
	simulateErr  error
	broadcastErr error
	hash         string
	height       int64
	simulates    int
	broadcasts   int
	account      SignerAccount
	accountErr   error
}

func (n *fakeNode) SignerAccount(context.Context, *std.Tx) (SignerAccount, error) {
	return n.account, n.accountErr
}

func (n *fakeNode) Simulate(tx *std.Tx) error {
	n.simulates++
	return n.simulateErr
}

func (n *fakeNode) Broadcast(tx *std.Tx) (string, int64, error) {
	n.broadcasts++
	return n.hash, n.height, n.broadcastErr
}

// signedFixture is the payment reqFixture prices, signed for real against the
// sign-doc inputs gno's auth ante would use, encoded the way a payload carries
// it, together with the on-chain account its signature verifies against.
// mutate, when non-nil, alters the transaction after it is signed.
//
// A request that must reach simulation needs this rather than txFixture: the
// facilitator verifies the signature against the signer's account, which
// txFixture's placeholder bytes cannot satisfy. The payer stays txFixture's
// address, since the account a payment is checked against is whichever one the
// node answers with.
func signedFixture(t *testing.T, mutate func(*std.Tx)) (txB64 string, account SignerAccount) {
	t.Helper()
	const (
		number   = 7
		sequence = 3
	)
	key := masterKey(t)
	tx := signedPayment{
		key:      key,
		chainID:  accountChainID,
		number:   number,
		sequence: sequence,
		from:     crypto.MustAddressFromString("g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5"),
	}.tx(t)
	if mutate != nil {
		mutate(tx)
	}
	raw, err := amino.Marshal(*tx)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw), accountAt(key, number, sequence)
}

func facilitatorRequestBody(t *testing.T, req PaymentRequirements, txB64 string) []byte {
	t.Helper()
	body, err := json.Marshal(Request{
		PaymentRequirements: req,
		PaymentPayload: PaymentPayload{
			X402Version: 2,
			Accepted:    req,
			Payload:     SchemePayload{Transaction: txB64},
		},
	})
	require.NoError(t, err)
	return body
}

func facilitatorRequest(t *testing.T, handler http.Handler, path string, req PaymentRequirements, txB64 string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(facilitatorRequestBody(t, req, txB64))))
	return rec
}

// postFacilitatorBody posts a body verbatim, for the malformed and ambiguous
// bodies no marshalled Request can express.
func postFacilitatorBody(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestFacilitator_VerifyValid(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	h := New(&fakeNode{account: account}, "dev").Handler()
	rec := facilitatorRequest(t, h, "/verify", reqFixture(), txB64)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.IsValid)
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", resp.Payer)
}

func TestFacilitator_VerifyWrongNetwork(t *testing.T) {
	h := New(&fakeNode{}, "other-chain").Handler()
	rec := facilitatorRequest(t, h, "/verify", reqFixture(), txFixture(t, nil)) // req says gno:dev
	require.Equal(t, http.StatusOK, rec.Code)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.IsValid)
	assert.Equal(t, ReasonInvalidNetwork, resp.InvalidReason)
}

// TestFacilitator_VerifySimulationFailure covers a chain that answered and
// refused. The node reports every refusal it decides as an ABCI error, which is
// what tells a refusal apart from a query that never reached it.
func TestFacilitator_VerifySimulationFailure(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	h := New(&fakeNode{account: account, simulateErr: std.ErrSessionExpired("session expired")}, "dev").Handler()
	rec := facilitatorRequest(t, h, "/verify", reqFixture(), txB64)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.IsValid)
	assert.Equal(t, ReasonSimulationFailed, resp.InvalidReason)
}

func TestFacilitator_VerifyLogsValidPayment(t *testing.T) {
	logs := captureLogs(t)
	txB64, account := signedFixture(t, nil)
	h := New(&fakeNode{account: account}, "dev").Handler()
	facilitatorRequest(t, h, "/verify", reqFixture(), txB64)

	record := logRecord(t, logs, "x402 verify: payment valid")
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", record["payer"])
}

func TestFacilitator_VerifyLogsInvalidPayment(t *testing.T) {
	t.Run("static rejection has no known payer", func(t *testing.T) {
		logs := captureLogs(t)
		h := New(&fakeNode{}, "dev").Handler()
		facilitatorRequest(t, h, "/verify", reqFixture(), "AAAA")

		record := logRecord(t, logs, "x402 verify: payment invalid")
		assert.Equal(t, ReasonMalformedTransaction, record["reason"])
		assert.NotContains(t, record, "payer")
	})

	t.Run("simulation rejection names the payer and the chain error", func(t *testing.T) {
		logs := captureLogs(t)
		txB64, account := signedFixture(t, nil)
		h := New(&fakeNode{account: account, simulateErr: std.ErrSessionExpired("session expired")}, "dev").Handler()
		facilitatorRequest(t, h, "/verify", reqFixture(), txB64)

		record := logRecord(t, logs, "x402 verify: payment invalid")
		assert.Equal(t, ReasonSimulationFailed, record["reason"])
		assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", record["payer"])
		assert.Contains(t, record["err"], "session expired")
		assert.NotContains(t, logs.String(), "simulation rejected", "one rejection is one log record")
	})
}

func TestFacilitator_SettleLogsInvalidPaymentUnderTheSettlePhase(t *testing.T) {
	t.Run("static rejection", func(t *testing.T) {
		logs := captureLogs(t)
		h := New(&fakeNode{}, "dev").Handler()
		facilitatorRequest(t, h, "/settle", reqFixture(), "AAAA")

		record := logRecord(t, logs, "x402 settle: payment invalid")
		assert.Equal(t, ReasonMalformedTransaction, record["reason"])
		assert.NotContains(t, record, "payer")
		assert.NotContains(t, logs.String(), "x402 verify", "a settle must not narrate itself as a verify")
	})

	t.Run("simulation rejection", func(t *testing.T) {
		logs := captureLogs(t)
		txB64, account := signedFixture(t, nil)
		h := New(&fakeNode{account: account, simulateErr: std.ErrInsufficientFunds("insufficient funds")}, "dev").Handler()
		facilitatorRequest(t, h, "/settle", reqFixture(), txB64)

		record := logRecord(t, logs, "x402 settle: payment invalid")
		assert.Equal(t, ReasonSimulationFailed, record["reason"])
		assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", record["payer"])
		assert.Contains(t, record["err"], "insufficient funds")
		assert.NotContains(t, logs.String(), "x402 verify", "a settle must not narrate itself as a verify")
	})
}

func TestFacilitator_SettleBroadcasts(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	node := &fakeNode{hash: "abc123", height: 42, account: account}
	h := New(node, "dev").Handler()
	rec := facilitatorRequest(t, h, "/settle", reqFixture(), txB64)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp SettleResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.Equal(t, "abc123", resp.Transaction)
	assert.Equal(t, "gno:dev", resp.Network)
	assert.Equal(t, 1, node.broadcasts)
}

func TestFacilitator_SettleRejectsInvalidWithoutBroadcast(t *testing.T) {
	node := &fakeNode{}
	h := New(node, "dev").Handler()
	rec := facilitatorRequest(t, h, "/settle", reqFixture(), "AAAA")
	var resp SettleResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
	assert.Equal(t, ReasonMalformedTransaction, resp.ErrorReason)
	assert.Equal(t, 0, node.broadcasts)
}

// TestFacilitator_SettleBroadcastFailure covers a broadcast the chain itself
// refused. The node reports every refusal it decides as an ABCI error, and only
// that makes the failed payment a verdict this endpoint may publish.
func TestFacilitator_SettleBroadcastFailure(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	node := &fakeNode{broadcastErr: std.ErrSessionExpired("session expired"), account: account}
	h := New(node, "dev").Handler()
	rec := facilitatorRequest(t, h, "/settle", reqFixture(), txB64)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp SettleResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
	assert.Equal(t, ReasonBroadcastFailed, resp.ErrorReason)
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", resp.Payer)
	assert.Equal(t, 1, node.broadcasts)
}

// TestFacilitator_SettleBroadcastUnreachableReportsNoVerdict pins the answer for
// a broadcast that never reached the chain.
//
// Such a failure is not a refusal. Upstream errors a broadcast both when CheckTx
// rejected the transaction and when it timed out waiting for the transaction to
// commit — and on the timeout the transaction is already in the mempool and will
// commit. Nothing at this layer separates the two beyond the chain's own
// abci.Error, so answering success:false would tell the seller a payment failed
// while the payer's funds move anyway, and the seller discards the response the
// payer paid for. 503 states what is actually known, and a client retries it.
func TestFacilitator_SettleBroadcastUnreachableReportsNoVerdict(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	node := &fakeNode{broadcastErr: errors.New("connection refused"), account: account}
	h := New(node, "dev").Handler()

	rec := facilitatorRequest(t, h, "/settle", reqFixture(), txB64)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotContains(t, rec.Body.String(), ReasonBroadcastFailed, "no reason code names an unknown outcome")
	assert.Equal(t, 1, node.broadcasts)
}

// TestFacilitator_SettleLogsTheTxOfAnAbortedDelivery pins the operator's record
// for a delivery the chain committed and then refused: such a transaction is on
// chain and charged the payer its fee, so its hash must reach the log even
// though the wire answer carries an empty transaction.
func TestFacilitator_SettleLogsTheTxOfAnAbortedDelivery(t *testing.T) {
	logs := captureLogs(t)
	txB64, account := signedFixture(t, nil)
	node := &fakeNode{hash: "abc123", height: 42, broadcastErr: std.ErrSessionExpired("session expired"), account: account}
	h := New(node, "dev").Handler()

	rec := facilitatorRequest(t, h, "/settle", reqFixture(), txB64)
	var resp SettleResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Transaction, "the spec's failed settlement carries an empty transaction")

	record := logRecord(t, logs, "x402 settle: broadcast failed")
	assert.Equal(t, "abc123", record["tx"])
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", record["payer"])
}

// TestFacilitator_VerifyRefusesATamperedSignature pins that verification decides
// the signature. gno's auth ante skips signature verification under simulate, so
// a forged signature simulates cleanly and would otherwise be reported valid,
// leaving a payer to discover at settle that the payment was never spendable.
func TestFacilitator_VerifyRefusesATamperedSignature(t *testing.T) {
	txB64, account := signedFixture(t, func(tx *std.Tx) { tx.Signatures[0].Signature[0] ^= 0xff })
	h := New(&fakeNode{account: account}, "dev").Handler()

	rec := facilitatorRequest(t, h, "/verify", reqFixture(), txB64)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.IsValid)
	assert.Equal(t, ReasonSignatureInvalid, resp.InvalidReason)
}

// TestFacilitator_VerifyRefusesAPayloadAcceptingADifferentOffer pins that the
// whole offer is compared, not the three fields that name a price.
//
// The scheme and the network are checked against the requirements, never against
// the payload's own accepted object, so a payload could agree about the price
// while naming another scheme or another chain. Upstream compares all five fields
// in its own matchers, and the signed transaction pins only recipient, amount and
// chain-id — so nothing mis-settles, but the facilitator would be asserting
// agreement about an offer the payload contradicts.
func TestFacilitator_VerifyRefusesAPayloadAcceptingADifferentOffer(t *testing.T) {
	txB64, account := signedFixture(t, nil)

	for name, accepted := range map[string]PaymentRequirements{
		"another scheme":  {Scheme: "upto", Network: "gno:dev", Amount: "250000", Asset: "ugnot", PayTo: reqFixture().PayTo},
		"another network": {Scheme: "exact", Network: "eip155:8453", Amount: "250000", Asset: "ugnot", PayTo: reqFixture().PayTo},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(Request{
				PaymentRequirements: reqFixture(),
				PaymentPayload: PaymentPayload{
					X402Version: 2,
					Accepted:    accepted,
					Payload:     SchemePayload{Transaction: txB64},
				},
			})
			require.NoError(t, err)

			h := New(&fakeNode{account: account}, "dev").Handler()
			rec := postFacilitatorBody(t, h, "/verify", string(body))
			require.Equal(t, http.StatusOK, rec.Code)
			var resp VerifyResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.IsValid)
			assert.Equal(t, ReasonInvalidPayload, resp.InvalidReason)
		})
	}
}

// TestFacilitator_SettleRefusalNamesThePayer pins that a refusal reports who the
// payment came from once verification decoded it. The broadcast-failure answer
// beside it already does, and the spec's own failure fixture carries a payer — a
// seller reading only the refusal would otherwise have no idea whose payment it
// was.
func TestFacilitator_SettleRefusalNamesThePayer(t *testing.T) {
	txB64, account := signedFixture(t, func(tx *std.Tx) { tx.Signatures[0].Signature[0] ^= 0xff })
	h := New(&fakeNode{account: account}, "dev").Handler()

	rec := facilitatorRequest(t, h, "/settle", reqFixture(), txB64)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp SettleResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
	assert.Equal(t, ReasonSignatureInvalid, resp.ErrorReason)
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", resp.Payer)
}

// TestFacilitator_SettleRefusalBeforeDecodingNamesNoPayer is the other half: a
// payload refused before the transaction decoded establishes no payer, and
// inventing one would name an address nothing verified.
func TestFacilitator_SettleRefusalBeforeDecodingNamesNoPayer(t *testing.T) {
	h := New(&fakeNode{}, "dev").Handler()

	rec := facilitatorRequest(t, h, "/settle", reqFixture(), "AAAA")
	var resp SettleResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
	assert.Equal(t, ReasonMalformedTransaction, resp.ErrorReason)
	assert.Empty(t, resp.Payer)
}

// TestFacilitator_VerifyRefusesAThresholdKeyWithoutVerifyingIt pins that a
// crafted threshold key is refused rather than verified, on both endpoints and
// with no credential of any kind.
//
// A threshold of zero passes the verifier's own bounds — it admits a signature
// list shorter than the bit array's set bits — and the verifier then indexes
// that list per set bit. Reaching it takes an account storing no key, which is
// what the bank keeper leaves behind on a first credit to an address, so the
// precondition is that someone once sent coins to the crafted key's address.
// Verification must therefore never hand such a key to VerifyBytes at all.
func TestFacilitator_VerifyRefusesAThresholdKeyWithoutVerifyingIt(t *testing.T) {
	multi := multisig.PubKeyMultisigThreshold{K: 0, PubKeys: []crypto.PubKey{ed25519.GenPrivKey().PubKey()}}
	// One set bit over an empty signature list: what the zero threshold admits.
	bits := bitarray.NewCompactBitArray(1)
	bits.SetIndex(0, true)
	sig := amino.MustMarshal(&multisig.Multisignature{BitArray: bits})

	txB64 := txFixture(t, func(tx *std.Tx) {
		send := tx.Msgs[0].(bank.MsgSend)
		// The signer is the address the crafted key derives, so the account read
		// resolves to it and the signature's own key is the one adopted.
		send.FromAddress = multi.Address()
		tx.Msgs[0] = send
		tx.Signatures[0] = std.Signature{PubKey: multi, Signature: sig}
	})
	// An account that exists and has never signed: it stores no key, so the ante
	// adopts the signature's.
	h := New(&fakeNode{account: accountWithKey(nil, 7, 3)}, "dev").Handler()

	for _, path := range []string{"/verify", "/settle"} {
		t.Run(path, func(t *testing.T) {
			rec := facilitatorRequest(t, h, path, reqFixture(), txB64)
			require.Equal(t, http.StatusOK, rec.Code)
			var resp struct {
				IsValid       bool   `json:"isValid"`
				Success       bool   `json:"success"`
				InvalidReason string `json:"invalidReason"`
				ErrorReason   string `json:"errorReason"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.IsValid)
			assert.False(t, resp.Success)
			assert.Equal(t, ReasonSignatureCount, resp.InvalidReason+resp.ErrorReason)
		})
	}
}

// TestFacilitator_VerifyRefusesAReplayedPayment pins that verification decides
// freshness. The account's sequence is the chain's own single-use record, so a
// signature valid over a sequence already consumed can only be a replay — and
// simulate passes it, verifying no signature at all.
func TestFacilitator_VerifyRefusesAReplayedPayment(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	// The chain consumed the sequence this payment signed over.
	consumed := accountWithKey(account.pubKey(), account.number(), account.sequence()+1)
	h := New(&fakeNode{account: consumed}, "dev").Handler()

	rec := facilitatorRequest(t, h, "/verify", reqFixture(), txB64)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.IsValid)
	assert.Equal(t, ReasonSequenceMismatch, resp.InvalidReason)
}

// TestFacilitator_AccountRejectionCostsNoSimulation pins the order of the two
// chain reads. /verify needs no credential and simulating is the expensive half,
// so a payment the account already refuses must be answered off the account read
// alone. The accepted case is asserted alongside, so the order cannot be
// satisfied by never simulating at all.
func TestFacilitator_AccountRejectionCostsNoSimulation(t *testing.T) {
	t.Run("a refused signature never reaches simulation", func(t *testing.T) {
		txB64, account := signedFixture(t, func(tx *std.Tx) { tx.Signatures[0].Signature[0] ^= 0xff })
		node := &fakeNode{account: account}
		facilitatorRequest(t, New(node, "dev").Handler(), "/verify", reqFixture(), txB64)
		assert.Zero(t, node.simulates)
	})

	t.Run("an accepted signature reaches it", func(t *testing.T) {
		txB64, account := signedFixture(t, nil)
		node := &fakeNode{account: account}
		facilitatorRequest(t, New(node, "dev").Handler(), "/verify", reqFixture(), txB64)
		assert.Equal(t, 1, node.simulates)
	})
}

// TestFacilitator_SettleRefusesWhatTheAccountRefuses keeps a doomed payment off
// the chain. CheckTx rejects both of these at settle, so broadcasting them buys
// nothing and hands an anonymous caller a free write attempt per request.
func TestFacilitator_SettleRefusesWhatTheAccountRefuses(t *testing.T) {
	cases := map[string]struct {
		mutate func(*std.Tx)
		// spent is how many further sequences the account consumed after this
		// payment was signed.
		spent      uint64
		wantReason string
	}{
		"tampered signature": {
			mutate:     func(tx *std.Tx) { tx.Signatures[0].Signature[0] ^= 0xff },
			wantReason: ReasonSignatureInvalid,
		},
		"consumed sequence": {
			spent:      1,
			wantReason: ReasonSequenceMismatch,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			txB64, account := signedFixture(t, tc.mutate)
			node := &fakeNode{account: accountWithKey(
				account.pubKey(), account.number(), account.sequence()+tc.spent)}
			rec := facilitatorRequest(t, New(node, "dev").Handler(), "/settle", reqFixture(), txB64)

			var resp SettleResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.Success)
			assert.Equal(t, tc.wantReason, resp.ErrorReason)
			assert.Zero(t, node.broadcasts, "a payment CheckTx would reject must never be broadcast")
		})
	}
}

// TestFacilitator_AbsentAccountIsAPaymentVerdict pins the one account-read
// failure that IS a verdict. gno's auth ante resolves the signer's account
// before it verifies anything, so a signer the chain holds no account for is a
// transaction neither simulation nor delivery would accept — the same refusal
// simulating would have reported, one chain read earlier. It shares that reason
// code, so the rejection carries the cause an operator needs to tell them apart.
func TestFacilitator_AbsentAccountIsAPaymentVerdict(t *testing.T) {
	logs := captureLogs(t)
	txB64, account := signedFixture(t, nil)
	node := &fakeNode{account: account, accountErr: std.ErrUnknownAddress("unknown address: g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5")}
	rec := facilitatorRequest(t, New(node, "dev").Handler(), "/verify", reqFixture(), txB64)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.IsValid)
	assert.Equal(t, ReasonSimulationFailed, resp.InvalidReason)

	record := logRecord(t, logs, "x402 verify: payment invalid")
	assert.Equal(t, ReasonSimulationFailed, record["reason"])
	assert.Contains(t, record["err"], "unknown address")
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", record["payer"])
	assert.Zero(t, node.simulates, "an account the chain refuses is answered without simulating")
}

// TestFacilitator_UnreachableChainIsNotAPaymentVerdict pins that a facilitator
// whose own chain access failed reports no verdict at all.
//
// 200 {isValid:false} states one, and under a node outage it states it about
// every payment that arrives — permanent-looking, so a client that believes it
// discards payments that are perfectly good. 503 says what actually happened,
// and an x402 client retries it. No scheme reason code is minted for the
// condition: this facilitator's own downtime does not belong in the published
// wire vocabulary.
func TestFacilitator_UnreachableChainIsNotAPaymentVerdict(t *testing.T) {
	unreachable := errors.New("query account: dial tcp 127.0.0.1:26657: connection refused")

	t.Run("verify answers no verdict", func(t *testing.T) {
		logs := captureLogs(t)
		txB64, account := signedFixture(t, nil)
		node := &fakeNode{account: account, accountErr: unreachable}
		rec := facilitatorRequest(t, New(node, "dev").Handler(), "/verify", reqFixture(), txB64)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.NotContains(t, rec.Body.String(), "isValid", "a 503 carries no verdict to believe")
		assert.Zero(t, node.simulates)

		record := logRecord(t, logs, "x402 verify: chain unreachable, reporting no verdict")
		assert.Contains(t, record["err"], "connection refused")
		assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", record["payer"])
	})

	t.Run("settle answers the same and broadcasts nothing", func(t *testing.T) {
		logs := captureLogs(t)
		txB64, account := signedFixture(t, nil)
		node := &fakeNode{account: account, accountErr: unreachable}
		rec := facilitatorRequest(t, New(node, "dev").Handler(), "/settle", reqFixture(), txB64)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Zero(t, node.broadcasts, "a payment the facilitator could not check must not be broadcast")
		logRecord(t, logs, "x402 settle: chain unreachable, reporting no verdict")
	})
}

// TestFacilitator_UnreachableChainAtSimulationIsNotAPaymentVerdict is the same
// distinction one chain read later. gnoclient reports a refused delivery and a
// query that never reached the node through one error return, and only the first
// says anything about the payment.
func TestFacilitator_UnreachableChainAtSimulationIsNotAPaymentVerdict(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	node := &fakeNode{account: account, simulateErr: errors.New("unable to perform ABCI query: connection refused")}
	rec := facilitatorRequest(t, New(node, "dev").Handler(), "/settle", reqFixture(), txB64)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, 1, node.simulates)
	assert.Zero(t, node.broadcasts, "a simulation that never reached the node refuses nothing")
}

// TestFacilitator_Supported pins all three required keys. extensions and
// signers are answered as empty rather than omitted: this facilitator is
// keyless, and "no signers" is a property worth stating.
func TestFacilitator_Supported(t *testing.T) {
	h := New(&fakeNode{}, "topaz-1").Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/supported", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp SupportedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Kinds, 1)
	assert.Equal(t, SupportedKind{X402Version: 2, Scheme: "exact", Network: "gno:topaz-1"}, resp.Kinds[0])
	assert.NotNil(t, resp.Extensions, "extensions is required, and empty is not absent")
	assert.Empty(t, resp.Extensions)
	assert.NotNil(t, resp.Signers, "signers is required, and a keyless facilitator publishes none")
	assert.Empty(t, resp.Signers)

	for _, field := range []string{`"kinds"`, `"extensions":[]`, `"signers":{}`} {
		assert.Contains(t, rec.Body.String(), field)
	}
}

// postFacilitatorRequest settles an arbitrary request body, for the cases the
// facilitatorRequest helper's well-formed shape cannot express.
func postFacilitatorRequest(t *testing.T, handler http.Handler, path string, fr Request) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(fr)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	return rec
}

// TestFacilitator_RejectsUnsupportedVersion pins that the payload's declared
// version is enforced. Left unread, a payload claiming 0, 1 or 99 verified
// identically to a v2 one.
func TestFacilitator_RejectsUnsupportedVersion(t *testing.T) {
	for _, version := range []int{0, 1, 3, 99} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			node := &fakeNode{}
			h := New(node, "dev").Handler()
			req := reqFixture()
			rec := postFacilitatorRequest(t, h, "/settle", Request{
				PaymentPayload: PaymentPayload{
					X402Version: version,
					Accepted:    req,
					Payload:     SchemePayload{Transaction: txFixture(t, nil)},
				},
				PaymentRequirements: req,
			})

			var resp SettleResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.Success)
			assert.Equal(t, ReasonInvalidVersion, resp.ErrorReason)
			assert.Zero(t, node.broadcasts, "an unsupported version must never settle")
		})
	}
}

// TestFacilitator_IgnoresAbsentTopLevelVersion pins that only the payload's
// version is enforced: a client that sets the payload version and omits the
// request's own must not be rejected for it.
func TestFacilitator_IgnoresAbsentTopLevelVersion(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	h := New(&fakeNode{account: account}, "dev").Handler()
	req := reqFixture()
	rec := postFacilitatorRequest(t, h, "/verify", Request{
		PaymentPayload: PaymentPayload{
			X402Version: 2,
			Accepted:    req,
			Payload:     SchemePayload{Transaction: txB64},
		},
		PaymentRequirements: req,
	})

	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.IsValid, "reason=%q", resp.InvalidReason)
}

// TestFacilitator_RejectsUnsupportedScheme pins that the scheme is read. Left
// unread, a requirements object claiming "upto" settled as "exact".
func TestFacilitator_RejectsUnsupportedScheme(t *testing.T) {
	for _, scheme := range []string{"upto", "", "EXACT", "permit"} {
		t.Run(scheme, func(t *testing.T) {
			node := &fakeNode{}
			h := New(node, "dev").Handler()
			req := reqFixture()
			req.Scheme = scheme
			rec := facilitatorRequest(t, h, "/settle", req, txFixture(t, nil))

			var resp SettleResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.Success)
			assert.Equal(t, ReasonUnsupportedScheme, resp.ErrorReason)
			assert.Zero(t, node.broadcasts, "an unsupported scheme must never settle")
		})
	}
}

// TestFacilitator_RejectsAcceptedMismatch cross-checks the payload's accepted
// object against the requirements the resource server actually asked for. The
// signed tx pins recipient and amount, so a mismatch cannot redirect funds —
// but a client and a seller that disagree about the price must be told so
// rather than have the disagreement silently resolved.
func TestFacilitator_RejectsAcceptedMismatch(t *testing.T) {
	cases := map[string]func(*PaymentRequirements){
		"payTo":  func(r *PaymentRequirements) { r.PayTo = "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5" },
		"amount": func(r *PaymentRequirements) { r.Amount = "1" },
		"asset":  func(r *PaymentRequirements) { r.Asset = "uatom" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			node := &fakeNode{}
			h := New(node, "dev").Handler()

			served := reqFixture()
			claimed := reqFixture()
			mutate(&claimed)

			rec := postFacilitatorRequest(t, h, "/settle", Request{
				PaymentPayload: PaymentPayload{
					X402Version: 2,
					Accepted:    claimed,
					Payload:     SchemePayload{Transaction: txFixture(t, nil)},
				},
				PaymentRequirements: served,
			})

			var resp SettleResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.Success)
			assert.Equal(t, ReasonInvalidPayload, resp.ErrorReason)
			assert.Zero(t, node.broadcasts, "a disputed offer must never settle")
		})
	}
}

// TestFacilitator_AcceptsMatchingAccepted proves the cross-check passes for a
// client that echoes the offer it was given, extra keys and all.
func TestFacilitator_AcceptsMatchingAccepted(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	h := New(&fakeNode{account: account}, "dev").Handler()
	served := reqFixture()
	served.MaxTimeoutSeconds = 60

	echoed := reqFixture()
	echoed.MaxTimeoutSeconds = 120                              // advisory, not part of the offer's identity
	echoed.Extra = map[string]any{"unknown": "echoed verbatim"} // ditto

	rec := postFacilitatorRequest(t, h, "/verify", Request{
		PaymentPayload: PaymentPayload{
			X402Version: 2,
			Accepted:    echoed,
			Payload:     SchemePayload{Transaction: txB64},
		},
		PaymentRequirements: served,
	})

	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.IsValid, "reason=%q", resp.InvalidReason)
}

// TestFacilitator_LogsWhichRequirementsMemoFailed pins the operational half of
// the single-code decision. A non-string memo and an over-cap memo both report
// invalid_payment_requirements, because both mean the same thing to a client —
// the seller's requirements are unusable. That collapse is only honest if an
// operator can still tell the two apart, so the rejection must carry the cause.
func TestFacilitator_LogsWhichRequirementsMemoFailed(t *testing.T) {
	cases := map[string]struct {
		extra     map[string]any
		wantCause string
	}{
		"memo is not a string": {
			extra:     map[string]any{"memo": 123},
			wantCause: "want string",
		},
		"memo is over the cap": {
			extra:     map[string]any{"memo": strings.Repeat("a", MaxMemoBytes+1)},
			wantCause: "exceeding the 256-byte maximum",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			node := &fakeNode{}
			h := New(node, "dev").Handler()

			req := reqFixture()
			req.Extra = tc.extra
			rec := facilitatorRequest(t, h, "/settle", req, txFixture(t, nil))

			var resp SettleResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.Success)
			assert.Equal(t, ReasonInvalidRequirements, resp.ErrorReason)
			assert.Zero(t, node.broadcasts, "unusable requirements must never settle")

			record := logRecord(t, logs, "x402 settle: payment invalid")
			assert.Equal(t, ReasonInvalidRequirements, record["reason"])
			cause, _ := record["err"].(string)
			assert.Contains(t, cause, tc.wantCause,
				"one wire reason covers both memo failures, so the log must name which one refused")
		})
	}
}

func TestFacilitator_RejectsMalformedBody(t *testing.T) {
	h := New(&fakeNode{}, "dev").Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/verify", bytes.NewReader([]byte("not json"))))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// decodeMarker is a number literal too large for the int field it targets:
// encoding/json quotes such a literal verbatim in its error text, so a body
// carrying it proves whether a rejection echoes the caller's own bytes.
const decodeMarker = "987654321987654321987654321987654321"

func TestFacilitator_MalformedBodyRejectionEchoesNothing(t *testing.T) {
	h := New(&fakeNode{}, "dev").Handler()
	rec := postFacilitatorBody(t, h, "/verify", `{"x402Version":`+decodeMarker+`}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid request body", strings.TrimSpace(rec.Body.String()),
		"an anonymous caller is told that the body was refused, and nothing more")
	assert.NotContains(t, rec.Body.String(), decodeMarker, "a rejected body must never be echoed back")
	assert.NotContains(t, rec.Body.String(), "cannot unmarshal", "the parser's error text is not for the caller")
}

func TestFacilitator_MalformedBodyIsLoggedWithoutEchoingIt(t *testing.T) {
	logs := captureLogs(t)
	h := New(&fakeNode{}, "dev").Handler()
	postFacilitatorBody(t, h, "/verify", `{"x402Version":`+decodeMarker+`}`)

	record := logRecord(t, logs, "x402: reject request body")
	assert.Equal(t, "malformed_body", record["detail"], "the operator must be able to tell why the body was refused")
	assert.NotContains(t, logs.String(), decodeMarker, "a rejected request body must never reach the logs")
}

// TestFacilitator_RejectsTrailingContent refuses a body that carries a second
// JSON value: json.Decoder stops at the first one, so an ambiguous body would
// otherwise let the facilitator and any other reader of the same bytes disagree
// about which payment was requested. Both endpoints are covered because settle
// moves funds on the answer.
func TestFacilitator_RejectsTrailingContent(t *testing.T) {
	trailing := map[string]string{
		"second JSON value": `{"x402Version":999}`,
		"garbage":           decodeMarker + "!",
	}
	for name, suffix := range trailing {
		t.Run(name, func(t *testing.T) {
			for _, endpoint := range []string{"verify", "settle"} {
				t.Run(endpoint, func(t *testing.T) {
					logs := captureLogs(t)
					node := &fakeNode{}
					h := New(node, "dev").Handler()
					body := string(facilitatorRequestBody(t, reqFixture(), txFixture(t, nil))) + suffix

					rec := postFacilitatorBody(t, h, "/"+endpoint, body)
					require.Equal(t, http.StatusBadRequest, rec.Code)
					assert.Equal(t, "invalid request body", strings.TrimSpace(rec.Body.String()))
					assert.Zero(t, node.broadcasts, "an ambiguous body must never settle")

					record := logRecord(t, logs, "x402: reject request body")
					assert.Equal(t, "trailing_content", record["detail"])
					assert.NotContains(t, logs.String(), decodeMarker, "a rejected request body must never reach the logs")
				})
			}
		})
	}
}

// TestFacilitator_AcceptsUnknownBodyFields pins forward compatibility: the
// protocol requires a field the facilitator does not know to be ignored, not
// refused, so the ambiguity check must reject only content outside the object.
func TestFacilitator_AcceptsUnknownBodyFields(t *testing.T) {
	txB64, account := signedFixture(t, nil)
	h := New(&fakeNode{account: account}, "dev").Handler()

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(facilitatorRequestBody(t, reqFixture(), txB64), &fields))
	fields["futureExtension"] = json.RawMessage(`{"unknown":true}`)
	body, err := json.Marshal(fields)
	require.NoError(t, err)

	rec := postFacilitatorBody(t, h, "/verify", string(body))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp VerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.IsValid, "reason=%q", resp.InvalidReason)
}
