package x402

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// Node is the chain access the facilitator needs: the signer's account for
// signature and sequence checks, full-tx simulation for verify, commit broadcast
// for settle. Implemented by the gnoclient adapter NewGnoclientNode and by fakes
// in tests.
//
// SignerAccount and Simulate each report a refusal the chain decided and a
// failure to reach the chain at all through one error, so an implementation has
// to preserve what it was handed: chainRefused tells the two apart by the
// chain's own abci.Error, and only that is a verdict on the payment.
type Node interface {
	AccountReader
	Simulate(tx *std.Tx) error
	Broadcast(tx *std.Tx) (hash string, height int64, err error)
}

// FacilitatorRequest is the body of POST /verify and POST /settle. A request
// that omits the top-level X402Version asserts no version; the payload's own
// version is the one verification enforces.
type FacilitatorRequest struct {
	X402Version         int                 `json:"x402Version,omitempty"`
	PaymentPayload      PaymentPayload      `json:"paymentPayload"`
	PaymentRequirements PaymentRequirements `json:"paymentRequirements"`
}

// Facilitator serves the x402 facilitator API for one gno chain.
type Facilitator struct {
	node    Node
	chainID string
	limiter *rateLimiter
}

// Option adjusts a Facilitator at construction.
type Option func(*Facilitator)

// WithRateLimit replaces the default per-remote-address throttle on /verify and
// /settle. Zero fields keep their defaults.
func WithRateLimit(rl RateLimit) Option {
	return func(f *Facilitator) { f.limiter = newRateLimiter(rl) }
}

// NewFacilitator wires the facilitator for one chain-id (the CAIP-2
// reference: network "gno:<chainID>"). The chain-touching endpoints are
// throttled at the default rate unless WithRateLimit says otherwise.
func NewFacilitator(node Node, chainID string, opts ...Option) *Facilitator {
	f := &Facilitator{node: node, chainID: chainID, limiter: newRateLimiter(RateLimit{})}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (f *Facilitator) network() string { return "gno:" + f.chainID }

// Handler returns the facilitator HTTP routes. Both endpoints that reach the
// chain are throttled; /supported answers from a constant and is not.
func (f *Facilitator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /verify", f.throttled(f.handleVerify))
	mux.Handle("POST /settle", f.throttled(f.handleSettle))
	mux.HandleFunc("GET /supported", f.handleSupported)
	return mux
}

// throttled fronts a chain-touching handler with the per-remote-address token
// bucket. Both endpoints answer anyone, and a payment that decodes costs a chain
// read before it can be refused — the signer's account, plus a simulation on top
// once the signature checks out — so an unthrottled endpoint lets a caller with
// no key at all spend the node's capacity at will.
func (f *Facilitator) throttled(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.limiter.allow(peerAddress(r)) {
			rejectRateLimited(w)
			return
		}
		next(w, r)
	})
}

// rejectRateLimited answers a caller that asked too often. The answer is fixed,
// and the record names no address: the key is whatever the connection reported,
// and this package logs no peer addresses.
func rejectRateLimited(w http.ResponseWriter) {
	slog.Info("x402: reject request", "detail", "rate_limited")
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}

// paymentVerdict is what a check concluded about a payment.
type paymentVerdict int

const (
	// paymentAccepted reports a payment nothing the check read refuses.
	paymentAccepted paymentVerdict = iota

	// paymentRefused reports a payment that will not settle, with a reason
	// naming why.
	paymentRefused

	// noVerdict reports that the check could not run, because the chain it
	// decides against could not be read. It is not a refusal: the payment is
	// neither valid nor invalid here, and saying either would state something
	// the checker does not know.
	noVerdict
)

// paymentCheck is what a facilitator concluded about one payment: the verdict,
// the decoded transaction when the payment is one to settle, the payer from the
// point the transaction decodes, the wire reason when it is refused, and the
// error behind a reason that covers several conditions or behind a failed read.
type paymentCheck struct {
	verdict paymentVerdict
	tx      *std.Tx
	payer   string
	reason  string
	cause   error
}

// refusePayment reports a payment this facilitator will not settle.
func refusePayment(payer, reason string, cause error) paymentCheck {
	return paymentCheck{verdict: paymentRefused, payer: payer, reason: reason, cause: cause}
}

// check runs the full verification (network match, static rules, the signer's
// account, simulate) and reports what it concluded. A payer is reported from the
// point the transaction decodes; every rejection before that leaves it empty.
// Logging is left to the calling handler, which knows the phase the rejection
// belongs to.
func (f *Facilitator) check(ctx context.Context, req FacilitatorRequest) paymentCheck {
	// The payload's own version is the one enforced; a request that omits the
	// top-level version asserts nothing about it.
	if req.PaymentPayload.X402Version != protocolVersion {
		return refusePayment("", ReasonInvalidVersion, nil)
	}
	if req.PaymentRequirements.Scheme != schemeExact {
		return refusePayment("", ReasonUnsupportedScheme, nil)
	}
	if req.PaymentRequirements.Network != f.network() {
		return refusePayment("", ReasonChainMismatch, nil)
	}
	if !acceptsSameOffer(req.PaymentPayload.Accepted, req.PaymentRequirements) {
		return refusePayment("", ReasonInvalidPayload, nil)
	}
	// VerifyStatic refuses an unusable memo too, but discards why. A non-string
	// memo and an over-cap one share one wire reason, so the cause is read here
	// to keep the rejection diagnosable.
	if _, err := req.PaymentRequirements.Memo(); err != nil {
		return refusePayment("", ReasonInvalidRequirements, err)
	}
	tx, payer, reason := VerifyStatic(req.PaymentRequirements, req.PaymentPayload.Payload)
	if reason != "" {
		return refusePayment("", reason, nil)
	}
	// The signer's account decides the signature and the sequence, and it decides
	// them before anything is simulated. Simulation cannot: gno's auth ante skips
	// signature verification under simulate, so a forged signature and a
	// superseded one both pass it and fail at settle. Answering off the account
	// read alone also keeps a payment that cannot settle from costing the node a
	// simulation, on an endpoint that asks the caller for no credential.
	switch verdict, reason, cause := verifyAccountChecks(ctx, tx, f.node, f.chainID); verdict {
	case noVerdict:
		return paymentCheck{verdict: noVerdict, payer: payer, cause: cause}
	case paymentRefused:
		return refusePayment(payer, reason, cause)
	}
	if err := f.node.Simulate(tx); err != nil {
		// A simulation that never reached the node refuses nothing, so it is not
		// answered as a refused payment either.
		if !chainRefused(err) {
			return paymentCheck{verdict: noVerdict, payer: payer, cause: err}
		}
		return refusePayment(payer, ReasonSimulationFailed, err)
	}
	return paymentCheck{verdict: paymentAccepted, tx: tx, payer: payer}
}

// verifyAccountChecks reports what the signer's on-chain account says about this
// payment. Simulation cannot answer it: gno's auth ante skips signature
// verification under simulate, so a forged signature and a superseded sequence
// both pass it and fail only at settle.
//
// An account read that failed reports no verdict rather than a refusal: nothing
// verified the signature, and answering "invalid" would blame the payment for the
// reader's own outage. An account the chain says does not exist is a refusal —
// the ante resolves the signer's account before it verifies anything, so such a
// transaction is one neither simulation nor delivery would accept, and it reports
// the code simulating would have reported. That code, and the signer-count one,
// cover more than one condition, so the cause is returned alongside: it is what
// an operator has to tell them apart. A refused signature carries none — the
// reason is the whole diagnosis.
func verifyAccountChecks(ctx context.Context, tx *std.Tx, reader AccountReader, chainID string) (verdict paymentVerdict, reason string, cause error) {
	signer, err := txSigner(tx)
	if err != nil {
		return paymentRefused, ReasonSignatureCount, err
	}
	acc, err := reader.SignerAccount(ctx, tx)
	if err != nil {
		if !chainRefused(err) {
			return noVerdict, "", err
		}
		return paymentRefused, ReasonSimulationFailed, err
	}
	switch verifySignature(tx, acc, signer, chainID) {
	case signatureFresh:
		return paymentAccepted, "", nil
	case signatureConsumed:
		return paymentRefused, ReasonSequenceMismatch, nil
	default:
		return paymentRefused, ReasonSignatureInvalid, nil
	}
}

// chainRefused reports whether a chain access error is the chain's own answer
// about the transaction rather than a failure to reach it.
//
// gno answers every refusal it decides — a signer with no account, a balance too
// low, an expired session, a delivery the VM aborted — with an abci.Error, the
// type the node puts in the response it returns. A dial failure, a timeout, an
// RPC-level error and an encoding change carry none, and none of them says
// anything about the payment. The test is by type on purpose: tm2's JSON-RPC
// layer flattens its own failures into one code whose prose is the only
// difference, and no answer here may rest on an upstream error string.
func chainRefused(err error) bool {
	_, ok := errors.AsType[abci.Error](err)
	return ok
}

// acceptsSameOffer reports whether the payload's accepted object names the same
// offer as the requirements it answers. Only the three fields that define the
// offer are compared: maxTimeoutSeconds is advisory and extra may legitimately
// carry keys either side does not know.
//
// The signed transaction pins recipient and amount, so a disagreement here
// cannot redirect funds — it means the client and the resource server priced the
// resource differently, which must be reported rather than silently resolved in
// the server's favor.
func acceptsSameOffer(accepted, required PaymentRequirements) bool {
	return accepted.PayTo == required.PayTo &&
		accepted.Amount == required.Amount &&
		accepted.Asset == required.Asset
}

// rejectionFields describes a rejected payment: the payer when verification
// decoded one, the reason code, and the chain error when one caused it.
func rejectionFields(payer, reason string, cause error) []any {
	var fields []any
	if payer != "" {
		fields = append(fields, "payer", payer)
	}
	fields = append(fields, "reason", reason)
	if cause != nil {
		fields = append(fields, "err", cause)
	}
	return fields
}

// rejectNoVerdict answers a caller the facilitator cannot decide for: its own
// chain access failed, so it holds nothing to report about the payment.
//
// 503 states that, and an x402 client retries it. A 200 carrying isValid:false
// would state a verdict instead — one that looks permanent, over every payment
// that arrives for as long as the node is unreachable — so a client believing it
// discards payments that may be perfectly good. No scheme reason code is minted
// for the condition either: a facilitator's own downtime does not belong in a
// published wire vocabulary.
func rejectNoVerdict(w http.ResponseWriter, phase, payer string, cause error) {
	var fields []any
	if payer != "" {
		fields = append(fields, "payer", payer)
	}
	fields = append(fields, "err", cause)
	slog.Error(phase+": chain unreachable, reporting no verdict", fields...)
	http.Error(w, "the payment could not be checked against the chain", http.StatusServiceUnavailable)
}

func (f *Facilitator) handleVerify(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFacilitatorRequest(w, r)
	if !ok {
		return
	}
	check := f.check(r.Context(), req)
	switch check.verdict {
	case noVerdict:
		rejectNoVerdict(w, "x402 verify", check.payer, check.cause)
		return
	case paymentRefused:
		slog.Info("x402 verify: payment invalid", rejectionFields(check.payer, check.reason, check.cause)...)
		writeJSON(w, VerifyResponse{IsValid: false, InvalidReason: check.reason})
		return
	}
	slog.Info("x402 verify: payment valid", "payer", check.payer)
	writeJSON(w, VerifyResponse{IsValid: true, Payer: check.payer})
}

func (f *Facilitator) handleSettle(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFacilitatorRequest(w, r)
	if !ok {
		return
	}
	check := f.check(r.Context(), req)
	switch check.verdict {
	case noVerdict:
		rejectNoVerdict(w, "x402 settle", check.payer, check.cause)
		return
	case paymentRefused:
		slog.Info("x402 settle: payment invalid", rejectionFields(check.payer, check.reason, check.cause)...)
		writeJSON(w, SettleResponse{Success: false, Network: f.network(), ErrorReason: check.reason})
		return
	}
	hash, height, err := f.node.Broadcast(check.tx)
	if err != nil {
		slog.Error("x402 settle: broadcast failed", "payer", check.payer, "err", err)
		writeJSON(w, SettleResponse{Success: false, Network: f.network(), Payer: check.payer, ErrorReason: ReasonBroadcastFailed})
		return
	}
	slog.Info("x402 settle: payment settled", "payer", check.payer, "tx", hash, "height", height)
	writeJSON(w, SettleResponse{Success: true, Transaction: hash, Network: f.network(), Payer: check.payer})
}

func (f *Facilitator) handleSupported(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, SupportedResponse{
		Kinds:      []SupportedKind{{X402Version: protocolVersion, Scheme: schemeExact, Network: f.network()}},
		Extensions: []string{},
		Signers:    map[string][]string{},
	})
}

// decodeFacilitatorRequest reads the request body both endpoints take, capped
// at 1 MiB.
func decodeFacilitatorRequest(w http.ResponseWriter, r *http.Request) (FacilitatorRequest, bool) {
	var req FacilitatorRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&req); err != nil {
		rejectRequestBody(w, "malformed_body")
		return FacilitatorRequest{}, false
	}
	// Decode reads the first JSON value and stops, so trailing content would be
	// dropped in silence: two readers of one body could then disagree about which
	// payment was requested. Unknown fields *inside* the object stay accepted —
	// the protocol requires a facilitator to tolerate what it does not know.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		rejectRequestBody(w, "trailing_content")
		return FacilitatorRequest{}, false
	}
	return req, true
}

// rejectRequestBody refuses a body the facilitator could not read as exactly
// one request. The answer is fixed: the decoder's error text quotes the caller's
// own bytes and the parser's internals, and this endpoint answers anyone. The
// detail names which check refused, and reaches the log only.
func rejectRequestBody(w http.ResponseWriter, detail string) {
	slog.Info("x402: reject request body", "detail", detail)
	http.Error(w, "invalid request body", http.StatusBadRequest)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("x402: write response", "err", err)
	}
}
