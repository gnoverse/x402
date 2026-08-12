package x402

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	bfttypes "github.com/gnolang/gno/tm2/pkg/bft/types"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/tmhash"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountNumber is the account number the signed fixtures below are signed for.
const accountNumber = 7

// fakeConfirmer is the chain view a seller reads: one account for the freshness
// check and one verdict per transaction hash. Lookups are counted under a mutex
// because the concurrency test reads them from another goroutine.
type fakeConfirmer struct {
	account    SignerAccount
	accountErr error

	// answerAfter delays the recorded verdict: the first answerAfter lookups
	// report NotCommitted, which is what a chain view looked at before the
	// transaction is indexed answers.
	answerAfter int

	mu         sync.Mutex
	confirmed  map[string]Confirmation
	confirmErr error
	lookups    int
}

func (c *fakeConfirmer) SignerAccount(context.Context, *std.Tx) (SignerAccount, error) {
	return c.account, c.accountErr
}

func (c *fakeConfirmer) ConfirmTx(_ context.Context, hash []byte) (Confirmation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups++
	if c.confirmErr != nil {
		return NotCommitted, c.confirmErr
	}
	if c.lookups <= c.answerAfter {
		return NotCommitted, nil
	}
	return c.confirmed[string(hash)], nil
}

func (c *fakeConfirmer) lookupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookups
}

// signedPaymentFor returns a payment signed at the given account sequence and
// the PAYMENT-SIGNATURE header carrying it. The transaction pays what
// reqFixture prices, so VerifyStatic accepts it against that fixture.
func signedPaymentFor(t *testing.T, key crypto.PrivKey, sequence uint64) (*std.Tx, string) {
	t.Helper()
	tx := signedPayment{key: key, chainID: accountChainID, number: accountNumber, sequence: sequence}.tx(t)
	return tx, signedPaymentHeader(t, tx)
}

// signedPaymentHeader is the PAYMENT-SIGNATURE header carrying tx, for the
// fixtures that alter a transaction after it is signed.
func signedPaymentHeader(t *testing.T, tx *std.Tx) string {
	t.Helper()
	return signedPaymentHeaderAccepting(t, tx, reqFixture())
}

// signedPaymentHeaderAccepting carries tx under a stated accepted object, for a
// seller whose offer is not the fixture's. The middleware matches accepted against
// its own options before it verifies anything, so a payload claiming the fixture
// against a seller offering something else is refused as unoffered — which is not
// the check those fixtures are trying to reach.
func signedPaymentHeaderAccepting(t *testing.T, tx *std.Tx, accepted PaymentRequirements) string {
	t.Helper()
	raw, err := amino.Marshal(tx)
	require.NoError(t, err)
	header, err := EncodePaymentHeader(PaymentPayload{
		X402Version: protocolVersion,
		Accepted:    accepted,
		Payload:     SchemePayload{Transaction: base64.StdEncoding.EncodeToString(raw)},
	})
	require.NoError(t, err)
	return header
}

// settleStub is a facilitator answering one fixed settle response, counting the
// requests that reached it — the only way to prove a check ran BEFORE settling.
type settleStub struct {
	*httptest.Server
	requests atomic.Int64
}

func newSettleStub(t *testing.T, response SettleResponse) *settleStub {
	t.Helper()
	stub := &settleStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests.Add(1)
		assert.NoError(t, json.NewEncoder(w).Encode(response))
	}))
	t.Cleanup(stub.Close)
	return stub
}

// confirmingConfig prices reqFixture behind the given confirmer. The window is
// squeezed to keep the tests quick: the production default spans block
// intervals, which no test can afford to wait out.
func confirmingConfig(facilitatorURL string, confirmer Confirmer) PaymentConfig {
	return PaymentConfig{
		Options:       []PaymentOption{{FacilitatorURL: facilitatorURL, Requirements: reqFixture()}},
		Confirmer:     confirmer,
		confirmWindow: 20 * time.Millisecond,
	}
}

// gatedHandler is RequirePayment around a handler that counts the requests
// reaching it. One instance per test, so concurrent requests share the
// middleware's in-flight guard.
type gatedHandler struct {
	http.Handler
	served atomic.Int64
}

func newGatedHandler(cfg PaymentConfig) *gatedHandler {
	g := &gatedHandler{}
	g.Handler = RequirePayment(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.served.Add(1)
		w.Write([]byte("premium"))
	}), cfg)
	return g
}

func paymentRequest(h http.Handler, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// serveWithPayment drives one request through a fresh gate and reports the
// response plus whether the gated handler ran.
func serveWithPayment(t *testing.T, cfg PaymentConfig, header string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	g := newGatedHandler(cfg)
	rec := paymentRequest(g, header)
	return rec, g.served.Load() == 1
}

func settleResponseHeader(t *testing.T, h http.Header) SettleResponse {
	t.Helper()
	raw := h.Get(PaymentResponseHeader)
	require.NotEmpty(t, raw, "an attached payment must report its outcome")
	data, err := base64.StdEncoding.DecodeString(raw)
	require.NoError(t, err, "PAYMENT-RESPONSE must be base64")
	var settle SettleResponse
	require.NoError(t, json.Unmarshal(data, &settle))
	return settle
}

// TestPaymentTxHash_MatchesTheChainsIndexKey pins the derivation against
// upstream's own hasher. The seller looks the payment up by a hash it derives,
// so a drift here silently turns every confirmation into "not committed".
func TestPaymentTxHash_MatchesTheChainsIndexKey(t *testing.T) {
	tx, _ := signedPaymentFor(t, masterKey(t), 3)
	wire, err := amino.Marshal(tx)
	require.NoError(t, err)

	hash, err := paymentTxHash(tx)
	require.NoError(t, err)
	assert.Equal(t, tmhash.Sum(wire), hash, "the chain keys its tx-result index on tmhash.Sum of the broadcast bytes")
	assert.Equal(t, bfttypes.Tx(wire).Hash(), hash, "which is exactly types.Tx.Hash")
}

// TestPaymentTxHash_IgnoresANonCanonicalEncoding pins the reason the
// transaction is re-marshalled instead of hashed as received. Amino accepts
// encodings it never produces, and the facilitator re-marshals the decoded
// transaction before broadcasting, so the chain indexes the canonical form
// whatever the client sent.
func TestPaymentTxHash_IgnoresANonCanonicalEncoding(t *testing.T) {
	tx, _ := signedPaymentFor(t, masterKey(t), 3)
	canonical, err := amino.Marshal(tx)
	require.NoError(t, err)

	// Memo is std.Tx's fourth field. Amino omits an empty string but accepts
	// one written out, so these bytes decode to the same transaction.
	padded := append(bytes.Clone(canonical), 0x22, 0x00)
	require.NotEqual(t, canonical, padded, "the padded form must differ on the wire")
	var decoded std.Tx
	require.NoError(t, amino.Unmarshal(padded, &decoded), "and must still decode")

	hash, err := paymentTxHash(&decoded)
	require.NoError(t, err)
	assert.Equal(t, tmhash.Sum(canonical), hash, "the hash must follow the bytes the facilitator broadcasts")
	assert.NotEqual(t, tmhash.Sum(padded), hash, "hashing the received bytes derives a hash no block carries")
}

// TestRequirePayment_ConfirmsANonCanonicallyEncodedPayment is the end-to-end
// half of the derivation: a client sends a padded encoding, the facilitator
// re-marshals it before broadcasting, and the chain indexes the canonical hash.
// A seller keying its lookup off the bytes it received would find nothing and
// refuse a payment that settled.
func TestRequirePayment_ConfirmsANonCanonicallyEncodedPayment(t *testing.T) {
	key := masterKey(t)
	tx, _ := signedPaymentFor(t, key, 3)
	canonical, err := amino.Marshal(tx)
	require.NoError(t, err)
	padded := append(bytes.Clone(canonical), 0x22, 0x00)
	header, err := EncodePaymentHeader(PaymentPayload{
		X402Version: protocolVersion,
		Accepted:    reqFixture(),
		Payload:     SchemePayload{Transaction: base64.StdEncoding.EncodeToString(padded)},
	})
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(tmhash.Sum(canonical)): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{Success: true, Network: "gno:dev", Payer: "g1payer"})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, served)
}

// TestRequirePayment_ConfirmedPaymentServes is the baseline the rest measures
// against: the chain shows the payment delivered, so the resource is served and
// PAYMENT-RESPONSE names the hash the seller verified rather than the one the
// facilitator reported.
func TestRequirePayment_ConfirmedPaymentServes(t *testing.T) {
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: "deadbeef", Network: "gno:dev", Payer: "g1payer",
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, served)
	assert.Equal(t, hex.EncodeToString(hash), settleResponseHeader(t, rec.Header()).Transaction,
		"the reported hash is attacker-chosen; the response must name the confirmed one")
}

// TestRequirePayment_UnconfirmedSettlementDoesNotServe is the check the whole
// feature exists for. A facilitator that answers success without broadcasting
// controls the seller's deliver-or-withhold decision over plaintext HTTP, so an
// unconfirmed settlement must never serve.
//
// The answer is 503 rather than 402 because the seller holds no payment
// verdict: the payment may have settled where this seller cannot see it, and an
// x402 client reads 402 as an invitation to pay again. Both arms — nothing
// committed, and a lookup that could not answer — take that one exit; no
// attempt is made to tell them apart, since the RPC flattens a missing result
// and any other handler failure into one code.
func TestRequirePayment_UnconfirmedSettlementDoesNotServe(t *testing.T) {
	key := masterKey(t)
	_, header := signedPaymentFor(t, key, 3)
	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: "deadbeef", Network: "gno:dev", Payer: "g1payer",
	})

	cases := map[string]*fakeConfirmer{
		"the payment never reached a block": {account: accountAt(key, accountNumber, 3)},
		"the lookup could not answer": {
			account:    accountAt(key, accountNumber, 3),
			confirmErr: errors.New("connection refused"),
		},
	}
	for name, confirmer := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)

			rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.False(t, served, "an unconfirmed settlement must not reach the gated handler")
			// Neither header may appear: PAYMENT-REQUIRED re-advertises the
			// offer, inviting a second payment for one that may have moved
			// funds, and PAYMENT-RESPONSE would have to name an outcome the
			// seller does not have.
			assert.Empty(t, rec.Header().Get(PaymentRequiredHeader))
			assert.Empty(t, rec.Header().Get(PaymentResponseHeader))
			assert.Greater(t, confirmer.lookupCount(), 1, "a reported success is given the window to appear")
			logRecord(t, logs, "x402 middleware: settlement unconfirmed on chain, refusing to serve")
		})
	}
}

// TestRequirePayment_ReportedFailureOverADeliveredPaymentServes covers the
// other direction of the same exposure: a facilitator that reports failure for
// a payment that did move funds. The buyer paid, so the resource is owed.
func TestRequirePayment_ReportedFailureOverADeliveredPaymentServes(t *testing.T) {
	logs := captureLogs(t)
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: false, Network: "gno:dev", Payer: "g1payer", ErrorReason: ReasonBroadcastFailed,
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, served)
	assert.Equal(t, 1, confirmer.lookupCount(), "the first decisive answer ends the lookup")

	settle := settleResponseHeader(t, rec.Header())
	assert.True(t, settle.Success, "the response must report the settlement that happened")
	assert.Empty(t, settle.ErrorReason)

	record := logRecord(t, logs, "x402 middleware: settlement reported failed for a payment the chain delivered")
	assert.Equal(t, hex.EncodeToString(hash), record["tx"])
	assert.Equal(t, ReasonBroadcastFailed, record["reportedReason"])
}

// TestRequirePayment_ConfirmedReceiptNamesTheDerivedPayer pins that a seller
// which checked the payment reports what it derived, not what it was told.
//
// The payer, the network and the transaction hash all come out of the signed
// bytes the seller holds, so relaying the facilitator's versions puts
// attacker-chosen values into the receipt the buyer reads and into the seller's
// own record of the sale — which is what revenue accounting, per-payer limits and
// refunds are built on. The reported hash was already discarded; these were not.
func TestRequirePayment_ConfirmedReceiptNamesTheDerivedPayer(t *testing.T) {
	logs := captureLogs(t)
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)
	payer := key.PubKey().Address().String()

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success:     true,
		Transaction: "deadbeef",
		Network:     "gno:someone-elses-chain",
		Payer:       "g1attackerchosenpayer",
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, served)
	settle := settleResponseHeader(t, rec.Header())
	assert.Equal(t, payer, settle.Payer, "the payer is the signer of the bytes the seller checked")
	assert.Equal(t, reqFixture().Network, settle.Network, "the network is the one this resource is priced on")
	assert.Equal(t, hex.EncodeToString(hash), settle.Transaction)

	record := logRecord(t, logs, "x402 middleware: payment accepted, serving content")
	assert.Equal(t, payer, record["payer"], "the seller's own record must not name an attacker's choice")
}

// TestRequirePayment_RefusedReceiptCarriesNoReportedHash covers the same
// substitution on the refusal path. Nothing was confirmed under the derived hash,
// so there is no transaction to name — and the facilitator's string is not one.
func TestRequirePayment_RefusedReceiptCarriesNoReportedHash(t *testing.T) {
	key := masterKey(t)
	_, header := signedPaymentFor(t, key, 3)
	payer := key.PubKey().Address().String()

	confirmer := &fakeConfirmer{account: accountAt(key, accountNumber, 3)}
	facilitator := newSettleStub(t, SettleResponse{
		Success:     false,
		Transaction: "deadbeef",
		Network:     "gno:someone-elses-chain",
		Payer:       "g1attackerchosenpayer",
		ErrorReason: ReasonSimulationFailed,
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	require.False(t, served)
	settle := settleResponseHeader(t, rec.Header())
	assert.Equal(t, ReasonSimulationFailed, settle.ErrorReason, "the reason is the facilitator's to report")
	assert.Equal(t, payer, settle.Payer)
	assert.Equal(t, reqFixture().Network, settle.Network)
	assert.Empty(t, settle.Transaction, "no transaction was confirmed, so none may be named")
}

// TestRequirePayment_ReportedFailureIsGivenTheWindowToLand closes the hole a
// single non-retried lookup left open.
//
// The reporting party chooses the instant the seller looks: broadcast, answer
// success:false immediately, and a lookup made before the transaction is indexed
// finds nothing. The buyer pays, the content is withheld, and the exposure is
// repeatable — the hold clears once the sequence advances, so the buyer signs and
// pays again. An honest facilitator meets the same race whenever
// BroadcastTxCommit times out over a transaction that still lands.
//
// A reported failure therefore gets the same window a reported success does. The
// physics are identical: a transaction that was broadcast appears in the seller's
// view a block or so later, whatever the facilitator claims about it.
func TestRequirePayment_ReportedFailureIsGivenTheWindowToLand(t *testing.T) {
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:     accountAt(key, accountNumber, 3),
		confirmed:   map[string]Confirmation{string(hash): Delivered},
		answerAfter: 2, // indexed only after the lookup the old code made
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: false, Network: "gno:dev", ErrorReason: ReasonBroadcastFailed,
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusOK, rec.Code, "a payment the chain delivered is owed the resource")
	assert.True(t, served)
	assert.Greater(t, confirmer.lookupCount(), 1, "one lookup lets the reporting party pick the instant")
	assert.True(t, settleResponseHeader(t, rec.Header()).Success)
}

// TestRequirePayment_ReportedFailureWithoutACorroboratingLookupIs503 separates a
// failure the seller's own chain view corroborates from one it could not check.
//
// A clean "no result for this hash", repeated across the window, is information:
// it agrees with the reported failure, so the seller holds a verdict and says so
// with a 402. A lookup that could not answer is not information — the seller
// learned nothing, exactly as when the account read fails before settling — and a
// 402 there asserts a verdict it lacks while inviting a second payment for one
// that may have moved funds.
func TestRequirePayment_ReportedFailureWithoutACorroboratingLookupIs503(t *testing.T) {
	key := masterKey(t)
	_, header := signedPaymentFor(t, key, 3)
	confirmer := &fakeConfirmer{
		account:    accountAt(key, accountNumber, 3),
		confirmErr: errors.New("dial tcp: connection refused"),
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: false, Network: "gno:dev", ErrorReason: ReasonSimulationFailed,
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, served)
	assert.Empty(t, rec.Header().Get(PaymentResponseHeader),
		"no settlement outcome was established, so none may be reported")
	assert.Empty(t, rec.Header().Get(PaymentRequiredHeader),
		"re-advertising the offer would invite a second payment")
}

// TestRequirePayment_ReportedFailureStandsWhenTheChainConfirmsNothing pins the
// asymmetry: the chain only ever overrides a reported failure in the buyer's
// favor. A chain view that answers cleanly and holds no result for the payment —
// the whole window through — corroborates the reported failure, so the seller has
// a verdict and reports it. Turning that into a 503 would leave a payer whose
// session expired or whose balance is short with no way to learn why, over every
// ordinary rejection.
func TestRequirePayment_ReportedFailureStandsWhenTheChainConfirmsNothing(t *testing.T) {
	key := masterKey(t)
	_, header := signedPaymentFor(t, key, 3)
	confirmer := &fakeConfirmer{account: accountAt(key, accountNumber, 3)}
	facilitator := newSettleStub(t, SettleResponse{
		Success: false, Network: "gno:dev", ErrorReason: ReasonSimulationFailed,
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.False(t, served)
	assert.Equal(t, ReasonSimulationFailed, settleResponseHeader(t, rec.Header()).ErrorReason)
	assert.Equal(t, confirmAttempts, confirmer.lookupCount(),
		"the window is spent before a failure the chain never recorded is accepted")
}

// TestRequirePayment_CommittedWithFailedDeliveryIs402 is the one positive
// payment verdict this path produces: the transaction is in a block and its
// delivery was refused, so no funds moved and the seller can say so.
func TestRequirePayment_CommittedWithFailedDeliveryIs402(t *testing.T) {
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): DeliveryFailed},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: "deadbeef", Network: "gno:dev", Payer: "g1payer",
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.False(t, served)
	settle := settleResponseHeader(t, rec.Header())
	assert.False(t, settle.Success)
	assert.Equal(t, ReasonBroadcastFailed, settle.ErrorReason)
	assert.Equal(t, hex.EncodeToString(hash), settle.Transaction, "the failed transaction is named by its confirmed hash")
}

// TestRequirePayment_ReplayedPaymentNeverReachesTheFacilitator pins the whole
// redesign. The account's sequence is the chain's own single-use record, so a
// signature that verifies one sequence back is proof the payment already
// settled. Refusing it before the settle call is what keeps this feature from
// creating the replay hole it would otherwise open: without a confirmer a
// replay already dies at settle, so the check has to run earlier, not merely
// somewhere.
func TestRequirePayment_ReplayedPaymentNeverReachesTheFacilitator(t *testing.T) {
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	// The account has moved on, and the payment is still committed and
	// delivered from its first use — so nothing but the sequence refuses it.
	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 4),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: "deadbeef", Network: "gno:dev", Payer: "g1payer",
	})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.False(t, served)
	assert.Zero(t, facilitator.requests.Load(), "a replay must die before it costs a settle call")
	assert.Zero(t, confirmer.lookupCount(), "and before any lookup")
	assert.Equal(t, ReasonSequenceMismatch, settleResponseHeader(t, rec.Header()).ErrorReason)
}

// TestRequirePayment_ForgedSignatureNeverReachesTheFacilitator is the same gate
// for a signature that verifies at no sequence at all.
func TestRequirePayment_ForgedSignatureNeverReachesTheFacilitator(t *testing.T) {
	key := masterKey(t)
	tx, _ := signedPaymentFor(t, key, 3)
	tx.Signatures[0].Signature[0] ^= 0xff
	raw, err := amino.Marshal(tx)
	require.NoError(t, err)
	header, err := EncodePaymentHeader(PaymentPayload{
		X402Version: protocolVersion,
		Accepted:    reqFixture(),
		Payload:     SchemePayload{Transaction: base64.StdEncoding.EncodeToString(raw)},
	})
	require.NoError(t, err)

	confirmer := &fakeConfirmer{account: accountAt(key, accountNumber, 3)}
	facilitator := newSettleStub(t, SettleResponse{Success: true, Network: "gno:dev"})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.False(t, served)
	assert.Zero(t, facilitator.requests.Load())
	assert.Equal(t, ReasonSignatureInvalid, settleResponseHeader(t, rec.Header()).ErrorReason)
}

// TestRequirePayment_PaymentMustMatchTheSellersOwnRequirements pins the check
// the derived hash cannot make. The hash binds the lookup to the payload; this
// binds the payload to what this endpoint priced. Without it a colluding
// facilitator broadcasts a payment paying its own address, reports success, and
// the lookup confirms a genuinely committed transaction — so the seller serves
// for a payment it never received.
func TestRequirePayment_PaymentMustMatchTheSellersOwnRequirements(t *testing.T) {
	key := masterKey(t)
	tx, _ := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	// The payment is genuinely committed and delivered: only the seller's own
	// requirements refuse it.
	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: hex.EncodeToString(hash), Network: "gno:dev", Payer: "g1payer",
	})

	cases := map[string]struct {
		requirements func(PaymentRequirements) PaymentRequirements
		wantReason   string
	}{
		"the payment pays another address": {
			requirements: func(r PaymentRequirements) PaymentRequirements {
				r.PayTo = "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5"
				return r
			},
			wantReason: ReasonRecipientMismatch,
		},
		"the payment pays another amount": {
			requirements: func(r PaymentRequirements) PaymentRequirements {
				r.Amount = "500000"
				return r
			},
			wantReason: ReasonAmountMismatch,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// The claim agrees with the seller's offer, so the request is matched to
			// it and only the signed transaction disagrees. That is the shape this
			// check exists for: an honest-looking accepted object over a payment that
			// pays somebody else. A claim naming a different offer never reaches here
			// — it is refused as unoffered, which is a different check.
			req := tc.requirements(reqFixture())
			cfg := confirmingConfig(facilitator.URL, confirmer)
			cfg.Options[0].Requirements = req

			rec, served := serveWithPayment(t, cfg, signedPaymentHeaderAccepting(t, tx, req))

			require.Equal(t, http.StatusPaymentRequired, rec.Code)
			assert.False(t, served, "a confirmed transaction that pays someone else buys nothing here")
			assert.Equal(t, tc.wantReason, settleResponseHeader(t, rec.Header()).ErrorReason)
		})
	}
}

// TestRequirePayment_ConcurrentDuplicateSettlesOnce covers the residual the
// freshness check leaves. It reads the sequence before the settle consumes it,
// so two requests carrying one payment can both find it fresh; the in-flight
// guard is what stops both from serving.
//
// The duplicate gets no verdict rather than a 402: while the first request is
// settling, the payment's outcome is genuinely unknown, and telling that client
// to pay again would strand the payment now being settled.
func TestRequirePayment_ConcurrentDuplicateSettlesOnce(t *testing.T) {
	logs := captureLogs(t)
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}

	// The facilitator holds the FIRST request inside the guarded window, so the
	// second must decide while the first is still settling. A later request is
	// answered at once, so a middleware that lets it through fails this test
	// rather than deadlocking it.
	var settles atomic.Int64
	settling := make(chan struct{}, 1)
	release := make(chan struct{})
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if settles.Add(1) == 1 {
			settling <- struct{}{}
			<-release
		}
		assert.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Transaction: hex.EncodeToString(hash), Network: "gno:dev", Payer: "g1payer",
		}))
	}))
	defer facilitator.Close()

	gate := newGatedHandler(confirmingConfig(facilitator.URL, confirmer))
	firstCode := make(chan int, 1)
	go func() { firstCode <- paymentRequest(gate, header).Code }()
	<-settling

	second := paymentRequest(gate, header)
	close(release)

	require.Equal(t, http.StatusOK, <-firstCode)
	require.Equal(t, http.StatusServiceUnavailable, second.Code)
	assert.Equal(t, int64(1), gate.served.Load(), "one payment buys the resource once")
	assert.Equal(t, int64(1), settles.Load(), "and is settled once")
	assert.Empty(t, second.Header().Get(PaymentResponseHeader),
		"the duplicate's outcome is unknown while the first is in flight, so none is reported")
	assert.Contains(t, logs.String(), detailSettlementInFlight,
		"the log has to name which unknown outcome this was")
}

// TestRequirePayment_ConcurrentDuplicateAcrossTwoGatesSettlesOnce extends the
// guard past one endpoint.
//
// Nothing binds a payment to a resource — VerifyStatic compares payTo, amount and
// asset, and a memo only when the seller sets one — so two priced endpoints with
// the same offer accept the same payment. A guard created inside RequirePayment
// gave each endpoint its own, so neither could see the other's claim and one
// payment bought both resources. Sequential reuse dies at the freshness check once
// the settle consumes the sequence; the concurrent case is what needs the guard,
// and it has to be process-wide to see it.
func TestRequirePayment_ConcurrentDuplicateAcrossTwoGatesSettlesOnce(t *testing.T) {
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}

	var settles atomic.Int64
	settling := make(chan struct{}, 1)
	release := make(chan struct{})
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if settles.Add(1) == 1 {
			settling <- struct{}{}
			<-release
		}
		assert.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Transaction: hex.EncodeToString(hash), Network: "gno:dev", Payer: "g1payer",
		}))
	}))
	defer facilitator.Close()

	cfg := confirmingConfig(facilitator.URL, confirmer)
	// Two separately-constructed gates, priced identically — two resources of one
	// seller, which is the ordinary shape.
	premium := newGatedHandler(cfg)
	archive := newGatedHandler(cfg)

	firstCode := make(chan int, 1)
	go func() { firstCode <- paymentRequest(premium, header).Code }()
	<-settling

	second := paymentRequest(archive, header)
	close(release)

	require.Equal(t, http.StatusOK, <-firstCode)
	require.Equal(t, http.StatusServiceUnavailable, second.Code,
		"the second gate must see the first gate's claim")
	assert.Equal(t, int64(1), premium.served.Load()+archive.served.Load(),
		"one payment buys one resource")
	assert.Equal(t, int64(1), settles.Load(), "and is settled once")
}

// TestRequirePayment_ConfirmerJudgesOnlyItsOwnChain pins the confirmer's scope, which is
// what lets one seller both confirm on chain and take a foreign asset.
//
// The confirmer reads a gno chain, so it can decide a gno payment and nothing else. Run
// over a matched option on another network it puts gno's own VerifyStatic in front of a
// payload that carries no gno transaction, and refuses every payment on that option —
// deleting the second network's whole purpose for any seller that also wanted
// confirmation. Since cmd/gnowars sets a confirmer unconditionally, that is every seller
// this repo ships.
//
// The consequence is stated rather than hidden: a foreign payment is decided on its
// facilitator's word, because this seller has no view of that chain to check it against.
func TestRequirePayment_ConfirmerJudgesOnlyItsOwnChain(t *testing.T) {
	evmReq := PaymentRequirements{Scheme: "exact", Network: "eip155:84532",
		Amount: "600000", Asset: "0xUSDC", PayTo: "0xSeller", MaxTimeoutSeconds: 60}

	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: "0xsettled", Network: "eip155:84532", Payer: "0xBuyer",
	})

	// Zero-valued: it holds no account and has confirmed nothing, so consulting it about
	// any payment refuses that payment. A served request therefore proves it was not
	// consulted, on top of the lookup count.
	confirmer := &fakeConfirmer{}
	g := newGatedHandler(PaymentConfig{
		Options:       []PaymentOption{{FacilitatorURL: facilitator.URL, Requirements: evmReq}},
		Confirmer:     confirmer,
		confirmWindow: 20 * time.Millisecond,
	})

	// A payload in the shape another chain's exact scheme sends: an authorization and a
	// signature, and no gno transaction anywhere in it.
	raw := `{"x402Version":2,"accepted":{"scheme":"exact","network":"eip155:84532",` +
		`"amount":"600000","asset":"0xUSDC","payTo":"0xSeller","maxTimeoutSeconds":60},` +
		`"payload":{"signature":"0xdeadbeef","authorization":{"from":"0xBuyer",` +
		`"to":"0xSeller","value":"600000","validAfter":"0","validBefore":"9999999999",` +
		`"nonce":"0xfeed"}}}`
	var payload PaymentPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	header, err := EncodePaymentHeader(payload)
	require.NoError(t, err)

	rec := paymentRequest(g, header)

	require.Equal(t, http.StatusOK, rec.Code,
		"a foreign payment must not be judged by a chain view that cannot see it")
	assert.Equal(t, int64(1), g.served.Load())
	assert.Equal(t, int64(1), facilitator.requests.Load(),
		"the foreign option settles at its own facilitator")
	assert.Zero(t, confirmer.lookups, "the gno chain must not be asked about a Base payment")

	settle := settleResponseHeader(t, rec.Header())
	assert.True(t, settle.Success)
	assert.Equal(t, "0xsettled", settle.Transaction,
		"with no confirmation of its own the seller reports the facilitator's hash")
}

// And the gno option in the same config is still confirmed, so adding a foreign option
// does not quietly downgrade the path that has a chain view.
func TestRequirePayment_ConfirmerStillJudgesTheGnoOption(t *testing.T) {
	key := masterKey(t)
	tx, header := signedPaymentFor(t, key, 3)
	hash, err := paymentTxHash(tx)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		account:   accountAt(key, accountNumber, 3),
		confirmed: map[string]Confirmation{string(hash): Delivered},
	}
	facilitator := newSettleStub(t, SettleResponse{
		Success: true, Transaction: "the facilitator's own claim", Network: "gno:dev",
	})

	evmReq := PaymentRequirements{Scheme: "exact", Network: "eip155:84532",
		Amount: "600000", Asset: "0xUSDC", PayTo: "0xSeller", MaxTimeoutSeconds: 60}
	g := newGatedHandler(PaymentConfig{
		Options: []PaymentOption{
			{FacilitatorURL: "http://unused.invalid", Requirements: evmReq},
			{FacilitatorURL: facilitator.URL, Requirements: reqFixture()},
		},
		Confirmer:     confirmer,
		confirmWindow: 20 * time.Millisecond,
	})

	rec := paymentRequest(g, header)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotZero(t, confirmer.lookups, "the gno option must still be confirmed on chain")
	assert.Equal(t, hex.EncodeToString(hash), settleResponseHeader(t, rec.Header()).Transaction,
		"the confirmed hash the seller derived replaces the facilitator's claim")
}

// TestRequirePayment_MalformedNetworkRefusesWithoutSettling covers a seller whose own
// requirements name a network that is not CAIP-2 at all.
//
// It used to be refused by the confirmer, on the reasoning that the sign doc covers the
// chain-id and no freshness check is possible without one. That reasoning stopped
// separating two cases once a seller could deliberately offer another chain: a network
// this scheme cannot read a gno chain-id out of is now usually a legitimate foreign
// option, decided on its own facilitator's word.
//
// What is left is the case that is still nobody's chain. It is refused earlier and harder
// than before — at pricing, with nothing advertised — because a seller that cannot say
// whose chain an offer belongs to has no business inviting a payment for it. Earlier
// matters: the old refusal arrived after the buyer had already signed.
func TestRequirePayment_MalformedNetworkRefusesWithoutSettling(t *testing.T) {
	key := masterKey(t)
	tx, _ := signedPaymentFor(t, key, 3)
	confirmer := &fakeConfirmer{account: accountAt(key, accountNumber, 3)}

	for name, network := range map[string]string{
		"no namespace":     "banana",
		"empty chain id":   "gno:",
		"empty namespace":  ":dev",
		"nothing whatever": "",
	} {
		t.Run(name, func(t *testing.T) {
			facilitator := newSettleStub(t, SettleResponse{Success: true, Network: "gno:dev"})
			req := reqFixture()
			req.Network = network
			cfg := confirmingConfig(facilitator.URL, confirmer)
			cfg.Options[0].Requirements = req

			rec, served := serveWithPayment(t, cfg, signedPaymentHeaderAccepting(t, tx, req))

			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"an offer belonging to no chain is the seller's fault, not a price to quote")
			assert.False(t, served)
			assert.Zero(t, facilitator.requests.Load(), "and nothing may be settled")
			assert.Empty(t, rec.Header().Get(PaymentRequiredHeader),
				"nor may it be advertised")
		})
	}
}

// TestRequirePayment_UnreachableChainAnswersWithoutAVerdict pins the seller's
// answer when its OWN chain view fails before any settlement is attempted. The
// freshness check that cannot run refuses rather than proceeds — a seller that
// settled here would be gating on nothing — but it refuses with no verdict.
//
// 402 would be two lies at once: it blames the payment for the seller's outage,
// and an x402 client reads it as an invitation to pay again. What makes that cost
// money is the retry of a payment that already settled — a payer re-signing on
// that invitation strands the one they already made on a sequence the chain has
// consumed, and pays twice for one resource.
func TestRequirePayment_UnreachableChainAnswersWithoutAVerdict(t *testing.T) {
	logs := captureLogs(t)
	key := masterKey(t)
	_, header := signedPaymentFor(t, key, 3)
	confirmer := &fakeConfirmer{accountErr: errors.New("query account: dial tcp 127.0.0.1:26657: connection refused")}
	facilitator := newSettleStub(t, SettleResponse{Success: true, Network: "gno:dev"})

	rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, confirmer), header)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, served, "a payment the seller could not check must not reach the gated handler")
	assert.Zero(t, facilitator.requests.Load(), "and must not be settled")
	// Neither header may appear, for the reasons writeSettlementUnconfirmed
	// states: one re-advertises the offer, the other names an outcome nothing
	// established.
	assert.Empty(t, rec.Header().Get(PaymentRequiredHeader))
	assert.Empty(t, rec.Header().Get(PaymentResponseHeader))

	record := logRecord(t, logs, "x402 middleware: cannot check the payment against the chain, refusing to serve")
	assert.Contains(t, record["err"], "connection refused",
		"an operator debugging the outage must see what failed, not a signature complaint")
}

// TestRequirePayment_RefusalPromptsTheRightRemedy pins that a 402 names what the
// chain decided. Three conditions reach it and a payer's remedy differs by case:
// sign a fresh payment, re-sign this one for this account, or have an account at
// all. One message for all of them tells most payers something untrue about
// their payment — and a signature complaint over a check that never ran sends a
// payer to re-sign what was never the problem.
func TestRequirePayment_RefusalPromptsTheRightRemedy(t *testing.T) {
	key := masterKey(t)
	_, header := signedPaymentFor(t, key, 3)
	forged, _ := signedPaymentFor(t, key, 3)
	forged.Signatures[0].Signature[0] ^= 0xff
	facilitator := newSettleStub(t, SettleResponse{Success: true, Network: "gno:dev"})

	cases := map[string]struct {
		header        string
		confirmer     *fakeConfirmer
		wantReason    string
		says          string
		staysSilentOn string
	}{
		"the chain consumed the sequence the payment signed over": {
			header:        header,
			confirmer:     &fakeConfirmer{account: accountAt(key, accountNumber, 4)},
			wantReason:    ReasonSequenceMismatch,
			says:          "sequence",
			staysSilentOn: "signature",
		},
		"the signature verifies at no sequence": {
			header:        signedPaymentHeader(t, forged),
			confirmer:     &fakeConfirmer{account: accountAt(key, accountNumber, 3)},
			wantReason:    ReasonSignatureInvalid,
			says:          "signature",
			staysSilentOn: "sequence",
		},
		"the chain holds no account for the signer": {
			header:        header,
			confirmer:     &fakeConfirmer{accountErr: std.ErrUnknownAddress("unknown address: g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5")},
			wantReason:    ReasonSimulationFailed,
			says:          "account",
			staysSilentOn: "signature",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec, served := serveWithPayment(t, confirmingConfig(facilitator.URL, tc.confirmer), tc.header)

			require.Equal(t, http.StatusPaymentRequired, rec.Code)
			assert.False(t, served)
			assert.Zero(t, facilitator.requests.Load(), "a payment the chain refuses costs no settle call")
			assert.Equal(t, tc.wantReason, settleResponseHeader(t, rec.Header()).ErrorReason)

			var body PaymentRequired
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Contains(t, body.Error, tc.says)
			assert.NotContains(t, body.Error, tc.staysSilentOn,
				"a payer must not be sent to fix what this refusal did not decide")
		})
	}
}

func TestChainIDFromNetwork(t *testing.T) {
	chainID, err := chainIDFromNetwork("gno:topaz-1")
	require.NoError(t, err)
	assert.Equal(t, "topaz-1", chainID)

	for _, network := range []string{"", "gno", "gno:", "eip155:1", "solana:mainnet"} {
		_, err := chainIDFromNetwork(network)
		assert.Error(t, err, "network %q names no gno chain-id", network)
	}
}

// A CAIP-2 name splits into exactly two parts, and every x402 client splits it
// that way to decide which mechanism handles the payment. A chain-id carrying its
// own colon therefore yields a network name no client can address, so reading a
// chain-id out of one would settle against a network the ecosystem cannot name.
func TestChainIDFromNetworkRejectsASecondColon(t *testing.T) {
	_, err := chainIDFromNetwork("gno:test:14")
	assert.Error(t, err)
}
