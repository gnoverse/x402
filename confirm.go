package x402

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto/tmhash"
	"github.com/gnolang/gno/tm2/pkg/std"
)

const (
	// defaultConfirmWindow bounds how long a reportedly-settled payment is
	// given to appear in the seller's own chain view.
	//
	// A seller sharing the node the facilitator broadcast through never waits:
	// the tx-result index is written in BlockExecutor.ApplyBlock before the
	// commit event BroadcastTxCommit waits on, so the lookup answers on the
	// first attempt. The window exists for a seller reading a different node,
	// where the answer arrives a block or so later — tm2's default commit
	// timeout is 5s, so this spans two block intervals plus slack. It is sized
	// generously on purpose: a payment that settled and is not seen in time is
	// unrecoverable (see Confirmer), so a lookup must not give up before a
	// healthy chain view could have caught up.
	defaultConfirmWindow = 12 * time.Second

	// confirmAttempts is how many lookups the window is divided into.
	confirmAttempts = 12

	// detailSettlementInFlight names a payment another request is settling.
	detailSettlementInFlight = "settlement_in_flight"
)

// Confirmation is what the chain records for a transaction hash.
type Confirmation int

const (
	// NotCommitted reports that the chain holds no result for the hash. A
	// result is written only once the transaction is in a block, so this covers
	// one still in the mempool, one CheckTx refused, and one never broadcast at
	// all.
	NotCommitted Confirmation = iota

	// DeliveryFailed reports a committed transaction whose delivery was
	// refused. No funds reached the recipient, but the payment is not free to
	// the payer and not reusable: a failed delivery still commits the ante's
	// writes, so the gas fee is deducted, a session's spend is charged against
	// its limit, and the sequence is consumed. That payment is permanently dead.
	DeliveryFailed

	// Delivered reports a committed transaction whose delivery succeeded.
	Delivered
)

// TxReader looks up a committed transaction by hash.
//
// A non-nil error means the lookup could not answer, which a caller must not
// read as NotCommitted. Neither answer may be served on, so no implementation
// has to separate a missing result from an unreachable node — a distinction
// tm2's RPC does not offer anyway, since it reports every handler failure under
// one internal-error code.
type TxReader interface {
	ConfirmTx(ctx context.Context, hash []byte) (Confirmation, error)
}

// Confirmer is the chain access a seller needs to decide a settlement for
// itself instead of taking the facilitator's word for it. It excludes Broadcast
// deliberately: a seller reads the chain, it never writes to it.
//
// Enabling confirmation carries a deployment requirement: the seller MUST read
// a chain view at least as current as the facilitator's, ideally the same node,
// where there is no race at all. Cross-node propagation lag is what the
// confirmation window absorbs.
//
// It is a requirement rather than advice because a payment that settles but is
// not confirmed in time cannot be recovered through the protocol: the freshness
// check refuses that payment on every later attempt, since the chain has
// consumed its sequence. There is deliberately no "consumed and committed and
// matching, so serve" recovery path — it reads as the humane fix, but a
// stateless middleware cannot express "once", so such a path would let a single
// payment fetch the resource forever.
//
// One residual belongs to the trust boundary rather than to this code: a
// facilitator can answer, receive the seller's refusal, and broadcast
// afterwards. The buyer then pays and is stranded. Nothing a keyless seller can
// check closes that, because the facilitator alone decides when it broadcasts.
type Confirmer interface {
	AccountReader
	TxReader
}

// paymentTxHash returns the hash the chain indexes this payment under:
// tmhash.Sum over its amino encoding, which is what ApplyBlock keys the
// tx-result index on and what BroadcastTxCommit submits.
//
// The transaction is re-marshalled rather than hashed as received, and the hash
// the facilitator reports is never used: that one is attacker-chosen, and it is
// absent entirely from a failure response. Amino accepts encodings it does not
// itself produce — a written-out empty field, a non-minimal length prefix — and
// decodes them to the same value, while the facilitator re-marshals the decoded
// transaction before broadcasting it. The chain therefore indexes the canonical
// form whatever the client sent, so hashing the bytes as received would derive a
// hash no block carries: correct on every honest request and wrong exactly when
// the client is hostile.
func paymentTxHash(tx *std.Tx) ([]byte, error) {
	wire, err := amino.Marshal(tx)
	if err != nil {
		return nil, fmt.Errorf("marshal payment transaction: %w", err)
	}
	return tmhash.Sum(wire), nil
}

// chainIDFromNetwork reads the chain-id out of a CAIP-2 gno network name, the id
// the signature's sign doc covers. It comes from the requirements the seller
// already publishes, so no second configuration field can disagree with them.
func chainIDFromNetwork(network string) (string, error) {
	chainID, ok := strings.CutPrefix(network, "gno:")
	if !ok || chainID == "" {
		return "", fmt.Errorf("network %q is not gno:<chain-id>", network)
	}
	return chainID, nil
}

// settlingPayments refuses a second concurrent settlement of one payment.
//
// Freshness is read before the settle consumes the sequence, so two requests
// carrying the same payment can both find it fresh and both serve for one
// payment. An entry lives only for the settle it guards, so the set is bounded
// by concurrent requests and needs no expiry.
//
// It is deliberately not a spent set. Single-use belongs to the account
// sequence, which the chain already enforces; a persistent set would stand up a
// weaker second copy of that authority, need a shared store and grow on demand
// from anyone willing to ask for 402s. Across replicas the residual stands: the
// same payer, replaying the same payment concurrently against two replicas,
// receives twice the resource they paid for once.
type settlingPayments struct {
	mu     sync.Mutex
	hashes map[string]struct{}
}

// processSettlements is the guard every gate shares, because a claim has to be
// visible to whoever might settle the same payment — and nothing binds a payment
// to a resource, so two endpoints with the same offer accept the same one. A guard
// per RequirePayment left each endpoint blind to the others' claims, and one
// payment bought both. Sequential reuse dies at the freshness check once the settle
// consumes the sequence; this covers the concurrent case, in this process.
var processSettlements = newSettlingPayments()

func newSettlingPayments() *settlingPayments {
	return &settlingPayments{hashes: make(map[string]struct{})}
}

// begin claims a hash for one request, reporting false when another request
// already holds it. release must be deferred, so a panic behind the middleware
// cannot leak the claim.
func (s *settlingPayments) begin(hash string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.hashes[hash]; held {
		return nil, false
	}
	s.hashes[hash] = struct{}{}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.hashes, hash)
	}, true
}

// settlementClaim is a payment cleared to settle: its signature is the one the
// chain will accept, it pays what this endpoint priced, and no other in-flight
// request is settling it.
type settlementClaim struct {
	reader TxReader
	hash   []byte
	// payer is the address the payment moves funds from, read out of the signed
	// bytes rather than out of the facilitator's answer.
	payer   string
	window  time.Duration
	release func()
}

// hexHash renders the derived hash the way the chain reports it.
func (c *settlementClaim) hexHash() string { return hex.EncodeToString(c.hash) }

// await asks the chain what became of the payment, until it answers something
// other than NotCommitted or the window runs out.
//
// Both reported outcomes get the window, and for one reason: the reporting party
// picks the instant the seller looks. Broadcasting and answering at once — as an
// honest facilitator does when BroadcastTxCommit times out over a transaction
// that still lands, and as a hostile one does deliberately — puts the lookup
// before the transaction is indexed. The physics do not depend on the claim: a
// transaction that was broadcast appears in the seller's view a block or so
// later either way, so a single lookup would let a reported failure win the race
// against a payment that moved funds.
//
// The cost is latency on an honestly-failed payment, which spends the window
// before it is refused. That is accepted: only a payment that already passed
// every seller-side check and was then refused at settle reaches here.
//
// The returned error is what separates a chain view that answered and held no
// result — information, which corroborates a reported failure — from one that
// could not answer, which is no verdict at all.
func (c *settlementClaim) await(ctx context.Context) (Confirmation, error) {
	interval := c.window / confirmAttempts
	var (
		state Confirmation
		err   error
	)
	for attempt := range confirmAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return NotCommitted, ctx.Err()
			case <-time.After(interval):
			}
		}
		if state, err = c.reader.ConfirmTx(ctx, c.hash); err == nil && state != NotCommitted {
			return state, nil
		}
	}
	return NotCommitted, err
}

// clearToSettle runs every check that must precede a settle attempt, and claims
// the payment for this request. A nil claim comes with the rejection to write.
//
// The transaction has to be decoded before its signature can be checked, so
// VerifyStatic runs first — against the seller's OWN requirements, which is the
// check the derived hash cannot make: the hash binds the lookup to the payload,
// this binds the payload to what this endpoint priced. Without it a colluding
// facilitator broadcasts a payment paying its own address, reports success, and
// the lookup confirms a genuinely committed transaction.
//
// Freshness then decides the payment before it can cost anything. The account's
// sequence is the chain's own single-use record, so a signature valid over a
// sequence already consumed can only be a replay, and refusing it here means no
// settle call and no broadcast attempt. That ordering is what keeps this feature
// from opening the replay hole it would otherwise create: a seller without a
// confirmer already refuses a replay, because the replay dies at settle.
//
// A non-nil error is neither a claim nor a rejection: the seller's own chain view
// would not answer, so it holds no verdict to serve on or to refuse with.
func clearToSettle(ctx context.Context, cfg PaymentConfig, option PaymentOption, payload PaymentPayload, settling *settlingPayments) (*settlementClaim, *rejection, error) {
	req := option.Requirements
	tx, payer, reason := VerifyStatic(req, payload.Payload)
	if reason != "" {
		return nil, refuseUnsettled(req, reason, "the payment does not satisfy the requirements for this resource"), nil
	}
	chainID, err := chainIDFromNetwork(req.Network)
	if err != nil {
		// The seller's own requirements are what cannot be acted on, so this
		// records as an operator error rather than a payer's.
		slog.Error("x402 middleware: cannot confirm payments for this resource", "network", req.Network, "err", err)
		return nil, refuseUnsettled(req, ReasonInvalidRequirements,
			"the payment could not be checked: this resource names no gno chain"), nil
	}
	switch verdict, reason, cause := verifyAccountChecks(ctx, tx, cfg.Confirmer, chainID); verdict {
	case noVerdict:
		return nil, nil, cause
	case paymentRefused:
		return nil, refuseUnsettled(req, reason, refusedPaymentMessage(reason)), nil
	}
	hash, err := paymentTxHash(tx)
	if err != nil {
		slog.Error("x402 middleware: derive payment transaction hash", "err", err)
		return nil, refuseUnsettled(req, ReasonMalformedTransaction,
			"the payment's transaction could not be re-encoded"), nil
	}
	release, ok := settling.begin(string(hash))
	if !ok {
		return nil, nil, errSettlementInFlight
	}
	return &settlementClaim{reader: cfg.Confirmer, hash: hash, payer: payer, window: cfg.confirmWindow, release: release}, nil, nil
}

// refusedPaymentMessage is the prose for a payment the seller's own chain view
// refuses, one message per condition the account read decides.
//
// They are not interchangeable: a payer's remedy differs by case — sign a fresh
// payment, re-sign this one against this chain and account, or have an account
// on chain at all — and prose that names the wrong check sends a payer to fix
// what was never the problem.
func refusedPaymentMessage(reason string) string {
	switch reason {
	case ReasonSequenceMismatch:
		return "the payment is stale: the chain has already consumed the sequence it was signed over"
	case ReasonSignatureInvalid:
		return "the payment's signature is not the one the chain would accept"
	case ReasonSimulationFailed:
		return "the chain holds no account for this payment's signer"
	default:
		return "the payment is not one the chain would accept"
	}
}

// refuseUnsettled refuses a payment before any settlement was attempted. The
// reason reaches the client through PAYMENT-RESPONSE, whose absence would
// otherwise read as "no payment was attached".
func refuseUnsettled(req PaymentRequirements, reason, message string) *rejection {
	return &rejection{
		status:  http.StatusPaymentRequired,
		message: message,
		reason:  reason,
		settle:  &SettleResponse{Network: req.Network, ErrorReason: reason},
	}
}

// errSettlementInFlight reports that another request holds this payment's claim.
// It travels as an error rather than a rejection because it is not a verdict: the
// payment's outcome is unknown precisely while that other request runs.
var errSettlementInFlight = errors.New("another request is settling this payment")

// writeSettlementInFlight answers a request for a payment another request is
// already settling. The outcome is unknown until that one finishes, so it is
// refused the same way an unreachable facilitator is, and the log's detail names
// which unknown outcome this was.
func writeSettlementInFlight(w http.ResponseWriter, r *http.Request) {
	refuseWithoutVerdict(w, "x402 middleware: payment already being settled, refusing to serve",
		"the payment is already being settled", "path", r.URL.Path, "detail", detailSettlementInFlight)
}

// refuseWithoutVerdict answers a request the seller holds no payment verdict on,
// without serving.
//
// It is deliberately not a 402: a 402 asserts a verdict, and an x402 client reads
// one as an invitation to pay again — which, for a payment that may already have
// moved funds, costs the buyer twice, since the chain has consumed the sequence
// the first one was signed over and no re-signed payment can redeem it. For the
// same reason the answer carries neither PAYMENT-REQUIRED, which would
// re-advertise the offer, nor PAYMENT-RESPONSE, which would have to name a
// settlement outcome that was never established.
//
// Every record is an error: each case is an operator's to act on.
func refuseWithoutVerdict(w http.ResponseWriter, record, message string, fields ...any) {
	slog.Error(record, fields...)
	http.Error(w, message, http.StatusServiceUnavailable)
}

// writeSettlementUnconfirmed answers a request whose payment the chain would not
// confirm: the seller has no verdict, because the payment may have settled where
// it cannot see it.
//
// Either the facilitator reported a settlement that did not happen, or the
// seller's chain view trails the one it settles through.
func writeSettlementUnconfirmed(w http.ResponseWriter, r *http.Request, hash string, cause error) {
	fields := []any{"path", r.URL.Path, "tx", hash}
	if cause != nil {
		fields = append(fields, "err", cause)
	}
	refuseWithoutVerdict(w, "x402 middleware: settlement unconfirmed on chain, refusing to serve",
		"the payment could not be confirmed on chain", fields...)
}

// writeChainUnreachable answers a request whose payment the seller could not
// check at all, before any settlement was attempted. Nothing about the payment
// was established — its signature was never verified — so a 402 would blame the
// payment for the seller's own outage.
//
// What makes that cost money rather than politeness is the retry of a payment
// that already settled: told its signature is bad, a payer signs another, and the
// settled one can never be redeemed afterwards, since the chain has consumed the
// sequence it was signed over. Answering with no verdict keeps the payer
// retrying the payment they already made, which a recovered chain view confirms.
func writeChainUnreachable(w http.ResponseWriter, r *http.Request, cause error) {
	refuseWithoutVerdict(w, "x402 middleware: cannot check the payment against the chain, refusing to serve",
		"the payment could not be checked against the chain", "path", r.URL.Path, "err", cause)
}
