package x402

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxPaymentHeaderSize bounds the base64-encoded PAYMENT-SIGNATURE header,
	// checked before it is decoded, so a client can't force large allocations. A
	// real header measures 816 bytes, 1644 with a memo at maxMemoBytes, so 16KiB
	// is ~10× the largest legitimate value. It also has to sit far below
	// http.DefaultMaxHeaderBytes (1<<20), which no server here raises: a cap up
	// there is all but unreachable, because net/http refuses a header past its
	// own limit with 431 and the client is then told nothing about x402.
	maxPaymentHeaderSize = 16 << 10

	// maxFacilitatorResponseSize bounds how much of the facilitator's
	// response body RequirePayment will read.
	maxFacilitatorResponseSize = 1 << 20 // 1MB

	// defaultFacilitatorTimeout bounds the settle request when the caller
	// doesn't supply a Client. A caller-supplied Client owns its own timeout
	// policy.
	defaultFacilitatorTimeout = 30 * time.Second

	// MaxPaymentDuration is the longest a gated request can take before
	// RequirePayment answers: the settle call plus, for a seller that configured a
	// Confirmer, the confirmation window.
	//
	// A seller must size its server's write timeout above this. Cutting a request
	// short after the settle has committed is unrecoverable — the payment moved
	// funds, and the payer cannot redeem it on a later request because the chain
	// consumed the sequence it was signed over. A caller supplying its own Client
	// substitutes that client's timeout for the settle half.
	MaxPaymentDuration = defaultFacilitatorTimeout + defaultConfirmWindow

	// defaultMaxTimeoutSeconds is advertised when the seller configures no
	// maxTimeoutSeconds. The field is always emitted, so leaving it zero would
	// advertise an offer a client reads as already expired.
	defaultMaxTimeoutSeconds = 60

	// Three transport failures share the wire reason invalid_payload, because
	// the spec gives the envelope one code for all of them. These details name
	// which check refused, for the log only — an operator still has to tell an
	// oversized header from an unparseable one.
	detailMalformedHeader = "malformed_payment_header"
	detailHeaderTooLarge  = "header_too_large"
	detailDuplicateHeader = "duplicate_payment_header"
)

// PaymentConfig configures RequirePayment for one priced resource.
type PaymentConfig struct {
	// Options are the ways to pay, advertised in this order. A client picks the
	// first it can act on, so the order is the seller's preference.
	Options []PaymentOption

	// OptionsFor prices one request, for a resource whose price depends on what
	// was asked for — a batch of N items at N × a unit price cannot be quoted by
	// a figure fixed at wiring time. Its result replaces Options for that request
	// alone, everywhere it matters: the advertised 402, the option a claim is
	// matched against, and the facilitator that settles it. Nil ⇒ Options prices
	// every request.
	//
	// It runs before anything can cost the buyer, so a seller must still refuse a
	// request it cannot fulfil ahead of this middleware rather than by returning
	// no options here — by the time a price exists, the buyer is being invited to
	// pay it.
	OptionsFor func(*http.Request) []PaymentOption

	// Client settles payments. Nil ⇒ a client with a default timeout. A supplied
	// Client owns its own timeout and transport policy, but not its redirect
	// policy: it is copied and made to refuse redirects, which is a security
	// property of the settle call rather than a caller preference.
	Client *http.Client

	// Confirmer decides settlements against the chain instead of on the
	// facilitator's word. Nil ⇒ the facilitator's word decides, which is what a
	// seller with no chain access must accept: making this mandatory would
	// destroy the RPC-less seller the scheme is designed around. Enabling it
	// carries a deployment requirement — see Confirmer.
	//
	// It covers the GNO options only. A gno chain view can neither decode nor look
	// up a payment settled on another chain, so an option on one is decided on its
	// facilitator's word even when this is set. A seller offering a foreign asset
	// is trusting that facilitator, and no amount of gno RPC changes it.
	Confirmer Confirmer

	// confirmWindow overrides defaultConfirmWindow. It is unexported because no
	// seller needs to tune the window, while the tests cannot wait out a real
	// one.
	confirmWindow time.Duration
}

// RequirePayment gates next behind an x402 payment: requests without a valid
// settled payment receive PaymentRequired, in the PAYMENT-REQUIRED header and
// the body; a request carrying a PAYMENT-SIGNATURE header is settled through the
// facilitator before next runs.
func RequirePayment(next http.Handler, cfg PaymentConfig) http.Handler {
	settleClient := http.Client{Timeout: defaultFacilitatorTimeout}
	if cfg.Client != nil {
		// A copy, so the caller keeps its own client and its own timeout policy
		// while the redirect policy below is not theirs to choose.
		settleClient = *cfg.Client
	}
	// Following a redirect would re-POST the signed payment wherever a 3xx points
	// and read whatever answers as a settlement verdict, so a hostile facilitator
	// URL — or an on-path 302 over the plaintext hop — would reach an arbitrary
	// host with a redeemable credential and decide this seller's deliver-or-
	// withhold for it. The 3xx is returned instead, and fails the status check.
	settleClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client := &settleClient
	if cfg.confirmWindow == 0 {
		cfg.confirmWindow = defaultConfirmWindow
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set before anything can write, so every answer below carries them.
		denyCaching(w)

		// A per-request copy of the config, shadowing the captured one. Pricing
		// below replaces Options for this request alone; assigning to the captured
		// cfg would publish one request's prices to every concurrent request, and a
		// cheap order would settle at an expensive one's amount.
		cfg := cfg
		if cfg.OptionsFor != nil {
			cfg.Options = cfg.OptionsFor(r)
		}
		offerable, ok := offerableOptions(cfg.Options)
		if !ok {
			refuseUnpriced(w, r)
			return
		}
		cfg.Options = offerable

		header := r.Header.Get(PaymentHeader)
		if header == "" {
			writePaymentRequired(w, r, cfg, rejection{
				status:  http.StatusPaymentRequired,
				message: PaymentHeader + " header is required",
			})
			return
		}
		// Header.Get reads the FIRST value, so an intermediary that prepends a
		// header would otherwise pick which of two attached payments settles.
		if len(r.Header.Values(PaymentHeader)) > 1 {
			writePaymentRequired(w, r, cfg, rejectAttached(http.StatusBadRequest,
				detailDuplicateHeader, "the request carries more than one "+PaymentHeader+" header"))
			return
		}
		if len(header) > maxPaymentHeaderSize {
			writePaymentRequired(w, r, cfg, rejectAttached(http.StatusBadRequest,
				detailHeaderTooLarge, "the "+PaymentHeader+" header is too large"))
			return
		}
		payload, err := DecodePaymentHeader(header)
		if err != nil {
			writePaymentRequired(w, r, cfg, rejectAttached(http.StatusBadRequest,
				detailMalformedHeader, "the "+PaymentHeader+" header is not a base64-encoded x402 payment payload"))
			return
		}

		// Which of the seller's offers the payment answers, decided from the
		// seller's own list. It runs before any check that reads requirements, so
		// the account checks and the settle call below both work from the seller's
		// copy of the terms rather than the client's claimed one.
		option, ok := matchOption(cfg.Options, payload.Accepted)
		if !ok {
			// The payload names an offer this seller did not make. Settling it would
			// let a client redeem terms it chose for itself.
			slog.Warn("x402 middleware: payload accepted an offer that was not advertised",
				"scheme", payload.Accepted.Scheme, "network", payload.Accepted.Network,
				"amount", payload.Accepted.Amount, "asset", payload.Accepted.Asset,
				"payTo", payload.Accepted.PayTo)
			writePaymentRequired(w, r, cfg, rejection{
				status:  http.StatusPaymentRequired,
				message: "the payment accepted an offer this resource does not make",
				reason:  ReasonInvalidPayload,
				// No network is named because no offer was selected, and the one
				// the client claimed is the client's own statement rather than
				// this seller's. What it can act on is the fresh accepts array
				// this refusal re-advertises.
				settle: &SettleResponse{ErrorReason: ReasonInvalidPayload},
			})
			return
		}

		// A configured Confirmer moves the deliver-or-withhold decision from the
		// facilitator's unauthenticated answer onto the chain. Every check that
		// must precede a settle attempt runs here, before the payment can cost
		// anything.
		//
		// It runs only for an option this Confirmer can actually speak about. A gno
		// chain view can neither decode nor look up a payment settled on another
		// chain, so applying it there would put gno's own VerifyStatic in front of a
		// payload carrying no gno transaction and refuse every payment on that
		// option. A foreign option is therefore decided on its facilitator's word —
		// which is the same position any seller without a chain view is in, and is
		// stated on Confirmer rather than left to be discovered.
		var claim *settlementClaim
		if cfg.Confirmer != nil && confirmable(option.Requirements.Network) {
			var (
				rej *rejection
				err error
			)
			claim, rej, err = clearToSettle(r.Context(), cfg, option, payload, processSettlements)
			switch {
			case errors.Is(err, errSettlementInFlight):
				writeSettlementInFlight(w, r)
				return
			case err != nil:
				writeChainUnreachable(w, r, err)
				return
			case rej != nil:
				writePaymentRequired(w, r, cfg, *rej)
				return
			}
			defer claim.release()
		}

		settle, ok := settlePayment(r, option, client, payload)
		if !ok {
			writeSettlementUnknown(w, r)
			return
		}

		if claim != nil {
			// Every field the seller can establish itself replaces the
			// facilitator's version of it, before any path can report one. The
			// payer comes out of the signed bytes, the network is what this
			// resource is priced on, and the hash is derived below once the chain
			// has recorded something under it — so a receipt, and the seller's own
			// record of the sale, never names an attacker's choice.
			settle.Payer = claim.payer
			settle.Network = option.Requirements.Network
			settle.Transaction = ""

			confirmed, cause := claim.await(r.Context())
			switch confirmed {
			case Delivered:
				if !settle.Success {
					// The chain is the authority both ways: a failure reported
					// over a payment that moved funds is overridden, because the
					// buyer paid and the resource is owed.
					slog.Error("x402 middleware: settlement reported failed for a payment the chain delivered",
						"tx", claim.hexHash(), "reportedReason", settle.ErrorReason)
					settle = SettleResponse{Success: true, Network: option.Requirements.Network, Payer: claim.payer}
				}
				settle.Transaction = claim.hexHash()
			case DeliveryFailed:
				// The one payment verdict this path produces: the transaction is
				// in a block and its delivery was refused. It moved no funds to
				// the seller, but it is not free to the payer — the ante's fee
				// deduction and sequence increment are committed even when
				// delivery is not — so the payment is dead rather than void.
				settle.Success = false
				settle.ErrorReason = ReasonBroadcastFailed
				settle.Transaction = claim.hexHash()
			default:
				// Nothing was confirmed, so a reported success buys nothing.
				// Confirmation states this build does not know land here too, so
				// an unrecognized one never serves.
				//
				// A reported failure turns on whether the chain view answered. A
				// clean "no result", the window through, corroborates it: the
				// seller holds a verdict and reports it, which is what leaves a
				// payer whose session expired or whose balance is short able to
				// learn why. A lookup that could not answer establishes nothing,
				// so it is refused the same way a failed account read is —
				// asserting a verdict there would invite a second payment for
				// one that may have moved funds.
				if settle.Success || cause != nil {
					writeSettlementUnconfirmed(w, r, claim.hexHash(), cause)
					return
				}
			}
		}

		if !settle.Success {
			writePaymentRequired(w, r, cfg, rejection{
				status:  http.StatusPaymentRequired,
				message: "the payment was not settled",
				reason:  settle.ErrorReason,
				settle:  &settle,
			})
			return
		}

		if err := setSettleHeader(w, settle); err != nil {
			// The payment settled and the client cannot be told so. A 402 would
			// invite a second payment for one that already moved funds.
			refuseWithoutVerdict(w, "x402 middleware: cannot report a settled payment, refusing to serve",
				"the settlement could not be reported", "path", r.URL.Path, "err", err)
			return
		}
		// Logged before serving: the payment has already moved funds on chain,
		// so it must be on record even if next panics.
		slog.Info("x402 middleware: payment accepted, serving content", "payer", settle.Payer, "tx", settle.Transaction)
		next.ServeHTTP(w, r)
	})
}

// offerableOptions defaults each option's advertised timeout and reports whether
// every one of them can be put in front of a buyer. It answers false rather than
// dropping the bad entry: a partial offer set is a different product from the one
// the operator configured, and silently serving the remainder hides the mistake.
// An empty list is the same answer — a resource with no way to pay for it is not
// priced, it is misconfigured.
func offerableOptions(options []PaymentOption) ([]PaymentOption, bool) {
	if len(options) == 0 {
		return nil, false
	}
	offerable := make([]PaymentOption, 0, len(options))
	for _, option := range options {
		if option.Requirements.MaxTimeoutSeconds == 0 {
			option.Requirements.MaxTimeoutSeconds = defaultMaxTimeoutSeconds
		}
		// An option no facilitator settles would be advertised and then answered
		// with an unknown outcome, so it is a misconfiguration rather than an offer.
		if option.FacilitatorURL == "" || !priceable(option.Requirements) {
			return nil, false
		}
		offerable = append(offerable, option)
	}
	return offerable, true
}

// priceable reports whether requirements can be offered to a buyer. Nothing is
// for sale without a positive amount, an asset, a payee, a network and a scheme,
// and a 402 quoting any of those empty asks for a payment no verification could
// match.
func priceable(req PaymentRequirements) bool {
	if req.Scheme == "" || req.Asset == "" || req.PayTo == "" {
		return false
	}
	// A network has to be CAIP-2, <namespace>:<reference>, because the namespace is
	// what says whose chain a payment belongs to. Without one the offer is nobody's:
	// no facilitator would recognise it, and this seller could not tell a deliberate
	// foreign option from a typo in its own — which is the difference between settling
	// on a facilitator's word and refusing to settle at all.
	if namespace, reference, found := strings.Cut(req.Network, ":"); !found || namespace == "" || reference == "" {
		return false
	}
	amount, err := strconv.ParseInt(req.Amount, 10, 64)
	return err == nil && amount > 0
}

// refuseUnpriced answers a request the seller's own pricing could not price.
//
// Not a 402: that would advertise requirements the seller never computed and
// invite a payment against them. Not a 503 either — no payment is in flight and
// nothing about this is transient. The seller was asked for a price and had none,
// which is its own fault and its own status.
func refuseUnpriced(w http.ResponseWriter, r *http.Request) {
	slog.Error("x402 middleware: cannot price the request, refusing to serve", "path", r.URL.Path)
	http.Error(w, "the request could not be priced", http.StatusInternalServerError)
}

// denyCaching keeps a shared cache from reselling one payment.
//
// A priced endpoint normally sits behind a CDN or a corporate proxy, and the
// credential rides PAYMENT-SIGNATURE rather than Authorization — so the rule
// that keeps a shared cache from storing a response to a credentialed request
// (RFC 9111 §3.5) never engages, and a stored 200 would serve one payer's
// content to everyone behind that cache for the freshness lifetime. Vary states
// the dependency for a cache that honors it; no-store withholds the response
// from one that only reads freshness. private additionally excludes a shared
// cache that ignores no-store.
//
// It applies to the refusals too: a cached 402 re-advertises requirements the
// seller may since have changed.
func denyCaching(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Vary", PaymentHeader)
}

// setSettleHeader reports a settlement outcome in PAYMENT-RESPONSE.
func setSettleHeader(w http.ResponseWriter, settle SettleResponse) error {
	encoded, err := json.Marshal(settle)
	if err != nil {
		return err
	}
	w.Header().Set(PaymentResponseHeader, base64.StdEncoding.EncodeToString(encoded))
	return nil
}

// confirmable reports whether a Confirmer could decide a payment on this network. It is
// the same question chainIDFromNetwork answers, asked without producing an error: a
// network this scheme cannot read a gno chain-id out of is one no gno chain view can
// confirm a payment on.
func confirmable(network string) bool {
	_, err := chainIDFromNetwork(network)
	return err == nil
}

// matchOption finds the seller's own copy of the offer a client claims to have
// accepted. It exists so everything downstream reads the seller's requirements
// rather than the client's: without it a client could accept the cheapest offer,
// hand back an object naming a different amount or recipient, and have that
// verified and settled instead.
//
// It extends acceptsSameOffer — the comparison the facilitator applies to the same
// pair — with scheme and network. The facilitator can leave those out because it
// serves one chain family and checks the network against its own chain; here they
// select which option, and therefore which facilitator, so an offer that agrees on
// price while naming another network is a different offer.
func matchOption(options []PaymentOption, accepted PaymentRequirements) (PaymentOption, bool) {
	for _, option := range options {
		if accepted.Scheme == option.Requirements.Scheme &&
			accepted.Network == option.Requirements.Network &&
			acceptsSameOffer(accepted, option.Requirements) {
			return option, true
		}
	}
	return PaymentOption{}, false
}

// settlePayment POSTs the settle request to the facilitator, using the
// server's own requirements rather than the client's claimed ones. On any
// transport/parse failure it returns ok=false; the caller writes the 402.
// It settles under r.Context(), so a client disconnect cancels the request:
// this can avoid the charge if the cancellation lands before the facilitator
// broadcasts, at the cost of the seller losing the settle outcome on a
// mid-settle cancel — that half-state exists regardless, since the broadcast
// itself is not cancelable once the facilitator has accepted it.
func settlePayment(r *http.Request, option PaymentOption, client *http.Client, payload PaymentPayload) (resp SettleResponse, ok bool) {
	body, err := json.Marshal(FacilitatorRequest{
		X402Version:         protocolVersion,
		PaymentPayload:      payload,
		PaymentRequirements: option.Requirements,
	})
	if err != nil {
		slog.Error("x402 middleware: marshal settle request", "err", err)
		return SettleResponse{}, false
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimSuffix(option.FacilitatorURL, "/")+"/settle", bytes.NewReader(body))
	if err != nil {
		slog.Error("x402 middleware: build settle request", "url", option.FacilitatorURL, "err", err)
		return SettleResponse{}, false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("x402 middleware: settle request canceled (client disconnect)", "url", option.FacilitatorURL, "err", err)
		} else {
			slog.Error("x402 middleware: settle request failed", "url", option.FacilitatorURL, "err", err)
		}
		return SettleResponse{}, false
	}
	defer httpResp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(httpResp.Body, maxFacilitatorResponseSize))
	if err != nil {
		slog.Error("x402 middleware: read settle response", "err", err)
		return SettleResponse{}, false
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		slog.Error("x402 middleware: facilitator returned error status", "status", httpResp.StatusCode)
		return SettleResponse{}, false
	}

	var settle SettleResponse
	if err := json.Unmarshal(data, &settle); err != nil {
		slog.Error("x402 middleware: parse settle response", "err", err)
		return SettleResponse{}, false
	}
	return settle, true
}

// rejection is one refused request.
//
// PaymentRequired.error is defined as human-readable prose, so message goes
// there and reason — the machine code — goes in PAYMENT-RESPONSE's errorReason
// instead. detail names which check refused and is logged only, since several
// checks legitimately share one wire reason. A nil settle means the request
// carried no payment at all, so no PAYMENT-RESPONSE is emitted; that absence is
// what tells an unpaid request apart from a payment that failed.
type rejection struct {
	status  int
	message string
	reason  string
	detail  string
	settle  *SettleResponse
}

// rejectAttached refuses a payment the middleware itself could not read. The
// envelope has one reason for all of these, so the detail carries which check
// refused.
//
// It names no network. The payload never decoded, so no offer was selected, and a
// resource with several of them has no single network to report — the client learns
// what is available from the accepts array this refusal carries.
func rejectAttached(status int, detail, message string) rejection {
	return rejection{
		status:  status,
		message: message,
		reason:  ReasonInvalidPayload,
		detail:  detail,
		settle:  &SettleResponse{ErrorReason: ReasonInvalidPayload},
	}
}

// writeSettlementUnknown answers a request whose settle call left the seller
// without an outcome: the facilitator could not be reached, answered a non-2xx,
// or returned a body that will not parse.
//
// The broadcast may have happened, so this is the same position a failed account
// read leaves the seller in — no verdict — and it gets the same answer. A 402 here
// asserted invalidity the seller never established and told an x402 client to pay
// again, which strands the first payment if it did settle: the chain has consumed
// the sequence it was signed over.
func writeSettlementUnknown(w http.ResponseWriter, r *http.Request) {
	refuseWithoutVerdict(w, "x402 middleware: settlement outcome unknown, refusing to serve",
		"the payment could not be settled", "path", r.URL.Path)
}

// writePaymentRequired answers a refused request. The status varies — a payload
// that cannot be parsed is 400, since 402 is reserved for verification and
// settlement failure and an x402 client retries on 402 — but PAYMENT-REQUIRED
// is always carried, or the client learns nothing it can act on.
func writePaymentRequired(w http.ResponseWriter, r *http.Request, cfg PaymentConfig, rej rejection) {
	// Every offer is logged, not just the first: which of them a buyer was refused
	// against is the question this line exists to answer, and a resource priced in
	// two assets has no single amount.
	offers := make([]string, len(cfg.Options))
	for i, option := range cfg.Options {
		offers[i] = option.Requirements.Network + " " + option.Requirements.Amount +
			" " + option.Requirements.Asset + " -> " + option.Requirements.PayTo
	}
	fields := []any{"path", r.URL.Path, "offers", offers}
	if rej.reason != "" {
		fields = append(fields, "reason", rej.reason)
	}
	if rej.detail != "" {
		fields = append(fields, "detail", rej.detail)
	}
	slog.Info("x402 middleware: payment required", fields...)

	accepts := make([]PaymentRequirements, len(cfg.Options))
	for i, option := range cfg.Options {
		accepts[i] = option.Requirements
	}
	body := PaymentRequired{
		X402Version: protocolVersion,
		Error:       rej.message,
		Resource:    &ResourceInfo{URL: r.URL.Path},
		Accepts:     accepts,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("x402 middleware: marshal payment required", "err", err)
	} else {
		w.Header().Set(PaymentRequiredHeader, base64.StdEncoding.EncodeToString(encoded))
	}
	if rej.settle != nil {
		if err := setSettleHeader(w, *rej.settle); err != nil {
			slog.Error("x402 middleware: marshal settle response header", "err", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rej.status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("x402 middleware: write payment required response", "err", err)
	}
}
