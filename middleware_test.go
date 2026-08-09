package x402

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oneOptionConfig is the single-way-to-pay shape: one offer, one facilitator. Most
// of these tests are about something other than having a choice, and spelling the
// slice out at every call site would bury what each one is actually pinning.
func oneOptionConfig(facilitatorURL string, req PaymentRequirements) PaymentConfig {
	return PaymentConfig{Options: []PaymentOption{{FacilitatorURL: facilitatorURL, Requirements: req}}}
}

func paymentHeader(t *testing.T, accepted PaymentRequirements) string {
	t.Helper()
	header, err := EncodePaymentHeader(PaymentPayload{
		X402Version: 2,
		Accepted:    accepted,
		Payload:     SchemePayload{Transaction: txFixture(t, nil)},
	})
	require.NoError(t, err)
	return header
}

// decodePaymentRequiredHeader decodes the PAYMENT-REQUIRED header, the spec's
// canonical location for the 402's payload.
func decodePaymentRequiredHeader(t *testing.T, h http.Header) PaymentRequired {
	t.Helper()
	raw := h.Get(PaymentRequiredHeader)
	require.NotEmpty(t, raw, "every rejection must carry PAYMENT-REQUIRED")
	data, err := base64.StdEncoding.DecodeString(raw)
	require.NoError(t, err, "PAYMENT-REQUIRED must be base64")
	var got PaymentRequired
	require.NoError(t, json.Unmarshal(data, &got))
	return got
}

// decodeSettleHeader decodes the PAYMENT-RESPONSE header, where a payment that was
// attached and refused reports its machine-readable reason.
func decodeSettleHeader(t *testing.T, h http.Header) SettleResponse {
	t.Helper()
	raw := h.Get(PaymentResponseHeader)
	require.NotEmpty(t, raw, "an attached payment must report its outcome")
	data, err := base64.StdEncoding.DecodeString(raw)
	require.NoError(t, err, "PAYMENT-RESPONSE must be base64")
	var got SettleResponse
	require.NoError(t, json.Unmarshal(data, &got))
	return got
}

// TestRequirePayment_RejectionPaths pins the status and the carriers for every
// way a request can fail to buy the resource.
//
// A payload the middleware cannot parse is a 400: 402 is reserved for
// verification and settlement failure, and an x402 client retries on 402, so
// answering 402 to an unparseable payload invites an endless retry. Every
// rejection still carries PAYMENT-REQUIRED, or the client learns nothing it can
// act on. PAYMENT-RESPONSE appears only where a payment was actually attached,
// which is what tells "no payment" apart from "the payment failed".
func TestRequirePayment_RejectionPaths(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: false, Network: "gno:dev", Payer: "g1payer", ErrorReason: ReasonSimulationFailed,
		}))
	}))
	defer rejecting.Close()

	valid := paymentHeader(t, reqFixture())

	cases := []struct {
		name           string
		facilitatorURL string
		header         func(*http.Request)
		wantStatus     int
		wantReason     string
		wantAttached   bool
	}{
		{
			name:           "no payment attached",
			facilitatorURL: "http://unused.invalid",
			header:         func(*http.Request) {},
			wantStatus:     http.StatusPaymentRequired,
		},
		{
			name:           "header is not base64",
			facilitatorURL: "http://unused.invalid",
			header:         func(r *http.Request) { r.Header.Set(PaymentHeader, "not-valid-base64!!!") },
			wantStatus:     http.StatusBadRequest,
			wantReason:     ReasonInvalidPayload,
			wantAttached:   true,
		},
		{
			name:           "header is base64 but not a payload",
			facilitatorURL: "http://unused.invalid",
			header:         func(r *http.Request) { r.Header.Set(PaymentHeader, "aGVsbG8=") },
			wantStatus:     http.StatusBadRequest,
			wantReason:     ReasonInvalidPayload,
			wantAttached:   true,
		},
		{
			name:           "header over the cap",
			facilitatorURL: "http://unused.invalid",
			header:         func(r *http.Request) { r.Header.Set(PaymentHeader, strings.Repeat("A", maxPaymentHeaderSize+1)) },
			wantStatus:     http.StatusBadRequest,
			wantReason:     ReasonInvalidPayload,
			wantAttached:   true,
		},
		{
			// Header.Get returns the FIRST value, so a prepending intermediary
			// could otherwise choose which of two payments is settled.
			name:           "two payment headers",
			facilitatorURL: "http://unused.invalid",
			header: func(r *http.Request) {
				r.Header.Add(PaymentHeader, valid)
				r.Header.Add(PaymentHeader, paymentHeader(t, reqFixture()))
			},
			wantStatus:   http.StatusBadRequest,
			wantReason:   ReasonInvalidPayload,
			wantAttached: true,
		},
		{
			name:           "settlement rejected the payment",
			facilitatorURL: rejecting.URL,
			header:         func(r *http.Request) { r.Header.Set(PaymentHeader, valid) },
			wantStatus:     http.StatusPaymentRequired,
			wantReason:     ReasonSimulationFailed,
			wantAttached:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("inner handler must not run for a rejected request")
			})
			h := RequirePayment(inner, oneOptionConfig(tc.facilitatorURL, reqFixture()))

			req := httptest.NewRequest(http.MethodGet, "/premium", nil)
			tc.header(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code)

			fromHeader := decodePaymentRequiredHeader(t, rec.Header())
			assert.Equal(t, 2, fromHeader.X402Version)
			require.Len(t, fromHeader.Accepts, 1)
			assert.Equal(t, reqFixture().PayTo, fromHeader.Accepts[0].PayTo)
			require.NotNil(t, fromHeader.Resource, "the 402 must name the resource")
			assert.Equal(t, "/premium", fromHeader.Resource.URL)

			var fromBody PaymentRequired
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fromBody), "the readable body is kept alongside the header")
			assert.Equal(t, fromHeader, fromBody, "header and body must describe the same offer")

			// PaymentRequired.error is defined as human-readable prose, so the
			// machine code belongs in PAYMENT-RESPONSE's errorReason and must
			// not leak into the body.
			assert.NotEmpty(t, fromBody.Error, "the body must say in prose why the request was refused")
			if tc.wantReason != "" {
				assert.NotContains(t, fromBody.Error, tc.wantReason,
					"the body's error is prose; the machine code belongs in PAYMENT-RESPONSE")
			}

			respHeader := rec.Header().Get(PaymentResponseHeader)
			if !tc.wantAttached {
				assert.Empty(t, respHeader, "a request that carried no payment has no settlement outcome to report")
				return
			}
			require.NotEmpty(t, respHeader, "a rejected payment that WAS attached must report its outcome")
			data, err := base64.StdEncoding.DecodeString(respHeader)
			require.NoError(t, err, "PAYMENT-RESPONSE must be base64")
			var settle SettleResponse
			require.NoError(t, json.Unmarshal(data, &settle))
			assert.False(t, settle.Success)
			assert.Equal(t, tc.wantReason, settle.ErrorReason)
		})
	}
}

// TestRequirePayment_UnknownSettleOutcomeIsNotAVerdict pins one answer for every
// way the settle call can leave the seller without an outcome.
//
// The seller's own chain-view failure already answers 503 on this reasoning: it
// holds no verdict, and a 402 both asserts one and tells an x402 client to pay
// again — which strands the first payment if it did settle, since the chain has
// consumed the sequence it was signed over. A facilitator that cannot be reached,
// answers a non-2xx, or returns a body that will not parse leaves the seller in
// exactly that position: the broadcast may have happened.
func TestRequirePayment_UnknownSettleOutcomeIsNotAVerdict(t *testing.T) {
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := unreachable.URL
	unreachable.Close()

	erroring := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer erroring.Close()

	unparseable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not json"))
	}))
	defer unparseable.Close()

	cases := map[string]string{
		"facilitator unreachable":   unreachableURL,
		"facilitator answers 500":   erroring.URL,
		"facilitator body unusable": unparseable.URL,
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			served := false
			h := RequirePayment(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served = true }),
				oneOptionConfig(url, reqFixture()),
			)
			req := httptest.NewRequest(http.MethodGet, "/premium", nil)
			req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.False(t, served)
			assert.Empty(t, rec.Header().Get(PaymentResponseHeader),
				"no settlement outcome was established, so none may be reported")
			assert.Empty(t, rec.Header().Get(PaymentRequiredHeader),
				"re-advertising the offer invites a second payment")
			assert.NotContains(t, rec.Body.String(), url,
				"the seller's own facilitator address is not the client's business")
		})
	}
}

// TestRequirePayment_SettleDoesNotFollowRedirects keeps the seller from being
// turned into a request forwarder.
//
// Following a redirect means re-POSTing the signed payment wherever a 3xx points
// and parsing whatever answers as a settlement verdict — so a hostile
// -facilitator URL, or an on-path 302 over the plaintext hop, reaches an arbitrary
// host with a redeemable credential and decides the seller's deliver-or-withhold
// for it. A caller-supplied Client keeps its own timeout policy; this one is not
// its to choose.
func TestRequirePayment_SettleDoesNotFollowRedirects(t *testing.T) {
	var elsewhereHit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHit.Store(true)
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Transaction: "abc123", Network: "gno:dev", Payer: "g1payer",
		}))
	}))
	defer elsewhere.Close()

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/settle", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	for name, client := range map[string]*http.Client{
		"default client":         nil,
		"caller-supplied client": {Timeout: defaultFacilitatorTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			elsewhereHit.Store(false)
			served := false
			h := RequirePayment(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served = true }),
				PaymentConfig{
					Options: []PaymentOption{{FacilitatorURL: redirecting.URL, Requirements: reqFixture()}},
					Client:  client,
				},
			)
			req := httptest.NewRequest(http.MethodGet, "/premium", nil)
			req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.False(t, elsewhereHit.Load(), "the payment must not be re-POSTed to a redirect target")
			assert.False(t, served)
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
				"a 3xx is not a settlement verdict")
		})
	}
}

// TestRequirePayment_ResponsesAreNotCacheable pins the two headers that keep an
// intermediary from reselling one payment.
//
// A priced endpoint normally sits behind a CDN or a corporate proxy, and the
// credential rides PAYMENT-SIGNATURE rather than Authorization — so the rule
// that keeps a shared cache from storing a credentialed response (RFC 9111
// §3.5) does not apply here, and a cache would serve one payer's content to
// everyone behind it for the freshness lifetime. Vary states that the response
// depends on the payment; no-store keeps it out of the cache regardless.
//
// It covers the 402 for the same reason in reverse: a cached 402 re-advertises
// requirements the seller may have changed.
func TestRequirePayment_ResponsesAreNotCacheable(t *testing.T) {
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Transaction: "abc123", Network: "gno:dev", Payer: "g1payer",
		}))
	}))
	defer facilitator.Close()

	cases := map[string]struct {
		header     func(*http.Request)
		wantStatus int
	}{
		"paid": {
			header:     func(r *http.Request) { r.Header.Set(PaymentHeader, paymentHeader(t, reqFixture())) },
			wantStatus: http.StatusOK,
		},
		"unpaid": {
			header:     func(*http.Request) {},
			wantStatus: http.StatusPaymentRequired,
		},
		"unreadable payment": {
			header:     func(r *http.Request) { r.Header.Set(PaymentHeader, "not-valid-base64!!!") },
			wantStatus: http.StatusBadRequest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := RequirePayment(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("premium")) }),
				oneOptionConfig(facilitator.URL, reqFixture()),
			)
			req := httptest.NewRequest(http.MethodGet, "/premium", nil)
			tc.header(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, "no-store, private", rec.Header().Get("Cache-Control"))
			assert.Equal(t, PaymentHeader, rec.Header().Get("Vary"))
		})
	}
}

func TestRequirePayment_No402Header(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler must not run without a payment")
	})
	cfg := oneOptionConfig("http://unused.invalid", reqFixture())
	h := RequirePayment(inner, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/premium", nil))

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	var body PaymentRequired
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Accepts, 1)
	assert.Equal(t, "exact", body.Accepts[0].Scheme)
}

// TestRequirePayment_DefaultsMaxTimeoutSeconds pins a non-zero advertised
// timeout. The field is always emitted, so a seller that configured none would
// otherwise advertise 0 — which a client computing a deadline reads as an
// already-expired offer, worse than an absent value.
func TestRequirePayment_DefaultsMaxTimeoutSeconds(t *testing.T) {
	req := reqFixture()
	require.Zero(t, req.MaxTimeoutSeconds, "the fixture configures none, so the middleware must supply one")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler must not run without a payment")
	})
	h := RequirePayment(inner, oneOptionConfig("http://unused.invalid", req))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/premium", nil))

	var body PaymentRequired
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Accepts, 1)
	assert.Equal(t, defaultMaxTimeoutSeconds, body.Accepts[0].MaxTimeoutSeconds)
}

// TestRequirePayment_KeepsConfiguredMaxTimeoutSeconds proves the default only
// fills a gap and never overrides the seller.
func TestRequirePayment_KeepsConfiguredMaxTimeoutSeconds(t *testing.T) {
	req := reqFixture()
	req.MaxTimeoutSeconds = 120

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := RequirePayment(inner, oneOptionConfig("http://unused.invalid", req))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/premium", nil))

	var body PaymentRequired
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Accepts, 1)
	assert.Equal(t, 120, body.Accepts[0].MaxTimeoutSeconds)
}

// TestRequirePayment_Logs402Requirements pins what an operator can reconstruct from
// a refusal: which resource, and every offer it was refused against. A resource
// priced in two assets has no single amount, so each offer is logged whole — logging
// only the first would make the log agree with a 402 the seller never sent.
func TestRequirePayment_Logs402Requirements(t *testing.T) {
	logs := captureLogs(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler must not run without a payment")
	})
	usdc := PaymentRequirements{Scheme: "exact", Network: "eip155:84532",
		Amount: "600000", Asset: "0xUSDC", PayTo: "0xSeller"}
	h := RequirePayment(inner, PaymentConfig{Options: []PaymentOption{
		{FacilitatorURL: "http://unused.invalid", Requirements: reqFixture()},
		{FacilitatorURL: "http://also-unused.invalid", Requirements: usdc},
	}})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/premium", nil))

	record := logRecord(t, logs, "x402 middleware: payment required")
	assert.Equal(t, "/premium", record["path"])
	offers, ok := record["offers"].([]any)
	require.True(t, ok, "the refusal must record what was on offer, got %#v", record["offers"])
	require.Len(t, offers, 2, "every offer, not just the first")
	for _, want := range []string{reqFixture().Amount, reqFixture().Asset, reqFixture().PayTo} {
		assert.Contains(t, offers[0], want)
	}
	for _, want := range []string{usdc.Amount, usdc.Asset, usdc.PayTo} {
		assert.Contains(t, offers[1], want)
	}
	assert.NotContains(t, record, "reason", "an unpaid request has no rejection reason")
}

// TestRequirePayment_LogsRejectionDetail pins the operational half of the
// vocabulary. Three distinct transport failures share the wire code
// invalid_payload, so the log carries a detail naming which check refused —
// without it an operator cannot tell an oversized header from an unparseable one.
func TestRequirePayment_LogsRejectionDetail(t *testing.T) {
	rejected := "not-valid-base64!!!"

	cases := []struct {
		name       string
		header     func(*http.Request)
		wantDetail string
	}{
		{
			name:       "unparseable",
			header:     func(r *http.Request) { r.Header.Set(PaymentHeader, rejected) },
			wantDetail: detailMalformedHeader,
		},
		{
			name:       "over the cap",
			header:     func(r *http.Request) { r.Header.Set(PaymentHeader, strings.Repeat("A", maxPaymentHeaderSize+1)) },
			wantDetail: detailHeaderTooLarge,
		},
		{
			name: "duplicated",
			header: func(r *http.Request) {
				r.Header.Add(PaymentHeader, "aGVsbG8=")
				r.Header.Add(PaymentHeader, "aGVsbG8=")
			},
			wantDetail: detailDuplicateHeader,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("inner handler must not run for a rejected header")
			})
			h := RequirePayment(inner, oneOptionConfig("http://unused.invalid", reqFixture()))

			req := httptest.NewRequest(http.MethodGet, "/premium", nil)
			tc.header(req)
			h.ServeHTTP(httptest.NewRecorder(), req)

			record := logRecord(t, logs, "x402 middleware: payment required")
			assert.Equal(t, ReasonInvalidPayload, record["reason"])
			assert.Equal(t, tc.wantDetail, record["detail"])
			assert.NotContains(t, logs.String(), rejected, "a rejected payment header must never reach the logs")
		})
	}
}

// TestRequirePayment_HeaderCapFiresOnARealServer drives an oversized header
// through a real server, where net/http parses the request headers itself.
// Calling the handler directly cannot exercise the cap's magnitude: net/http
// never runs there, so a cap sitting at http.DefaultMaxHeaderBytes still looks
// enforced while a real server answers 431 first and the client is told nothing
// about x402. The refusal has to come from the cap, and it has to name which
// check refused — the wire reason is shared, so the log's detail is the only
// place an operator can tell an oversized header from an unparseable one.
func TestRequirePayment_HeaderCapFiresOnARealServer(t *testing.T) {
	// An absolute size rather than maxPaymentHeaderSize+1: the claim under test
	// is that a header this small is already refused, by the cap and not by
	// net/http.
	const oversized = 17 << 10

	logs := captureLogs(t)
	innerRan := false
	srv := httptest.NewServer(RequirePayment(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { innerRan = true }),
		oneOptionConfig("http://unused.invalid", reqFixture()),
	))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/premium", nil)
	require.NoError(t, err)
	req.Header.Set(PaymentHeader, strings.Repeat("A", oversized))
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	// Close waits for the served request, so the handler goroutine's log records
	// and innerRan are settled before this goroutine reads them.
	srv.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"431 means net/http refused the header first, leaving the client no x402 reason")
	record := logRecord(t, logs, "x402 middleware: payment required")
	assert.Equal(t, ReasonInvalidPayload, record["reason"])
	assert.Equal(t, detailHeaderTooLarge, record["detail"])
	assert.False(t, innerRan, "the inner handler must not run for a refused header")
}

func TestRequirePayment_LogsAcceptedPaymentWithoutLeakingTheHeader(t *testing.T) {
	logs := captureLogs(t)
	settleResp := SettleResponse{Success: true, Transaction: "abc123", Network: "gno:dev", Payer: "g1payer"}
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(settleResp))
	}))
	defer facilitator.Close()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := oneOptionConfig(facilitator.URL, reqFixture())
	h := RequirePayment(inner, cfg)

	header := paymentHeader(t, reqFixture())
	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, header)
	h.ServeHTTP(httptest.NewRecorder(), req)

	record := logRecord(t, logs, "x402 middleware: payment accepted, serving content")
	assert.Equal(t, "g1payer", record["payer"])
	assert.Equal(t, "abc123", record["tx"])
	assert.NotContains(t, logs.String(), header, "the signed payment header must never reach the logs")
	assert.NotContains(t, logs.String(), txFixture(t, nil), "the signed transaction must never reach the logs")
}

func TestRequirePayment_LogsAcceptedPaymentWhenTheHandlerPanics(t *testing.T) {
	logs := captureLogs(t)
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Transaction: "abc123", Network: "gno:dev", Payer: "g1payer",
		}))
	}))
	defer facilitator.Close()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	cfg := oneOptionConfig(facilitator.URL, reqFixture())
	h := RequirePayment(inner, cfg)

	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
	assert.Panics(t, func() { h.ServeHTTP(httptest.NewRecorder(), req) })

	// The payment already moved funds on chain, so the settled payment must be
	// on record whatever the handler behind the middleware does.
	record := logRecord(t, logs, "x402 middleware: payment accepted, serving content")
	assert.Equal(t, "g1payer", record["payer"])
	assert.Equal(t, "abc123", record["tx"])
}

func TestRequirePayment_SettlesAndServes(t *testing.T) {
	settleResp := SettleResponse{Success: true, Transaction: "abc123", Network: "gno:dev", Payer: "g1payer"}
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/settle", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(settleResp))
	}))
	defer facilitator.Close()

	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
		w.Write([]byte("premium"))
	})
	cfg := oneOptionConfig(facilitator.URL, reqFixture())
	h := RequirePayment(inner, cfg)

	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "premium", rec.Body.String())
	assert.True(t, innerCalled)

	respHeader := rec.Header().Get(PaymentResponseHeader)
	require.NotEmpty(t, respHeader)
	data, err := base64.StdEncoding.DecodeString(respHeader)
	require.NoError(t, err)
	var got SettleResponse
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, settleResp, got)
}

// TestRequirePayment_NoConfirmerTakesTheFacilitatorsWord is the regression
// guard for every seller that configures no Confirmer. A payment nothing on
// chain would confirm — the fixture transaction carries a placeholder signature
// and was never broadcast — is served purely because the facilitator said so.
// That is the trust boundary an RPC-less seller accepts, and confirmation must
// remain opt-in rather than quietly become mandatory.
func TestRequirePayment_NoConfirmerTakesTheFacilitatorsWord(t *testing.T) {
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Transaction: "deadbeef", Network: "gno:dev", Payer: "g1payer",
		}))
	}))
	defer facilitator.Close()

	cfg := oneOptionConfig(facilitator.URL, reqFixture())
	require.Nil(t, cfg.Confirmer, "the default seller checks nothing on chain")

	innerCalled := false
	h := RequirePayment(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { innerCalled = true }), cfg)
	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, innerCalled)
	assert.Equal(t, "deadbeef", settleResponseHeader(t, rec.Header()).Transaction,
		"with nothing verified, the reported hash is reported verbatim")
}

func TestRequirePayment_FailedSettleIs402(t *testing.T) {
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: false, Network: "gno:dev", ErrorReason: ReasonSimulationFailed,
		}))
	}))
	defer facilitator.Close()

	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { innerCalled = true })
	cfg := oneOptionConfig(facilitator.URL, reqFixture())
	h := RequirePayment(inner, cfg)

	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.False(t, innerCalled)

	// The facilitator's reason reaches the client through PAYMENT-RESPONSE, not
	// the body, whose error field is prose.
	raw := rec.Header().Get(PaymentResponseHeader)
	require.NotEmpty(t, raw)
	data, err := base64.StdEncoding.DecodeString(raw)
	require.NoError(t, err)
	var settle SettleResponse
	require.NoError(t, json.Unmarshal(data, &settle))
	assert.Equal(t, ReasonSimulationFailed, settle.ErrorReason)
	assert.NotContains(t, rec.Body.String(), ReasonSimulationFailed)
}

// TestRequirePayment_ServerRequirementsOverrideClientClaim pins that nothing a
// client writes in its accepted object reaches the facilitator. The two halves are
// different mechanisms and both are needed.
//
// A claim that names another recipient is refused outright, before any settle call:
// the offer it accepted is not one this seller makes. A claim agreeing on the offer
// but disagreeing on the advisory fields is matched — maxTimeoutSeconds and extra are
// deliberately outside the comparison, because either side may legitimately carry
// keys the other does not know — and the settle call then carries the SELLER's copy
// of them. That matters most for extra.memo, which binds the transaction: a client
// able to substitute its own memo could have a payment verified against a binding the
// seller never set.
func TestRequirePayment_ServerRequirementsOverrideClientClaim(t *testing.T) {
	serverReq := reqFixture()
	serverReq.Extra = map[string]any{"memo": "the seller's own binding"}

	var gotReq atomic.Value
	var settles atomic.Int64
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settles.Add(1)
		var fr FacilitatorRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&fr))
		gotReq.Store(fr.PaymentRequirements)
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{Success: true, Network: "gno:dev"}))
	}))
	defer facilitator.Close()

	h := RequirePayment(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		oneOptionConfig(facilitator.URL, serverReq),
	)
	serve := func(claim PaymentRequirements) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/premium", nil)
		req.Header.Set(PaymentHeader, paymentHeader(t, claim))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("the advisory fields are the seller's, not the client's", func(t *testing.T) {
		claim := reqFixture()
		claim.Extra = map[string]any{"memo": "a memo the client chose for itself"}
		claim.MaxTimeoutSeconds = 1

		require.Equal(t, http.StatusOK, serve(claim).Code)
		settled, ok := gotReq.Load().(PaymentRequirements)
		require.True(t, ok)
		assert.Equal(t, serverReq.Extra, settled.Extra,
			"the memo the payment is verified against must be the seller's")
		assert.Equal(t, defaultMaxTimeoutSeconds, settled.MaxTimeoutSeconds)
	})

	t.Run("a claim naming another recipient never reaches the facilitator", func(t *testing.T) {
		before := settles.Load()
		claim := reqFixture()
		claim.PayTo = "g1attacker00000000000000000000000000000"

		rec := serve(claim)
		assert.Equal(t, http.StatusPaymentRequired, rec.Code)
		assert.Equal(t, before, settles.Load(),
			"an offer the seller never made must be refused without settling")
	})
}

func TestRequirePayment_FacilitatorURLTrailingSlashNotDoubled(t *testing.T) {
	var gotPath string
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{Success: true, Network: "gno:dev"}))
	}))
	defer facilitator.Close()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := oneOptionConfig(facilitator.URL+"/", reqFixture())
	h := RequirePayment(inner, cfg)

	req := httptest.NewRequest(http.MethodGet, "/premium", nil)
	req.Header.Set(PaymentHeader, paymentHeader(t, reqFixture()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/settle", gotPath)
}

// TestMatchOption is the rule that stops a client paying for the cheap offer and
// redeeming the expensive one: the accepted object it sends back must equal one of
// the offers the seller actually made, and settlement then uses the seller's copy.
func TestMatchOption(t *testing.T) {
	ugnot := PaymentOption{
		FacilitatorURL: "http://gno-facilitator",
		Requirements: PaymentRequirements{
			Scheme: "exact", Network: "gno:dev", Amount: "250000",
			Asset: "ugnot", PayTo: "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
		},
	}
	usdc := PaymentOption{
		FacilitatorURL: "http://evm-facilitator",
		Requirements: PaymentRequirements{
			Scheme: "exact", Network: "eip155:84532", Amount: "1000",
			Asset: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", PayTo: "0xSeller",
		},
	}
	options := []PaymentOption{ugnot, usdc}

	t.Run("matches the second offer", func(t *testing.T) {
		got, ok := matchOption(options, usdc.Requirements)
		require.True(t, ok)
		assert.Equal(t, "http://evm-facilitator", got.FacilitatorURL)
	})

	t.Run("refuses an amount the seller never offered", func(t *testing.T) {
		cheap := usdc.Requirements
		cheap.Amount = "1"
		_, ok := matchOption(options, cheap)
		assert.False(t, ok, "a client must not redeem an offer the seller did not make")
	})

	t.Run("refuses a network the seller never offered", func(t *testing.T) {
		other := usdc.Requirements
		other.Network = "eip155:1"
		_, ok := matchOption(options, other)
		assert.False(t, ok)
	})

	t.Run("refuses a scheme the seller never offered", func(t *testing.T) {
		other := usdc.Requirements
		other.Scheme = "permit"
		_, ok := matchOption(options, other)
		assert.False(t, ok, "the scheme decides how the payload is read")
	})

	t.Run("refuses a payTo the seller never offered", func(t *testing.T) {
		redirected := usdc.Requirements
		redirected.PayTo = "0xAttacker"
		_, ok := matchOption(options, redirected)
		assert.False(t, ok, "the recipient is the whole point of the offer")
	})

	t.Run("refuses an asset the seller never offered", func(t *testing.T) {
		other := usdc.Requirements
		other.Asset = "0xWorthlessToken"
		_, ok := matchOption(options, other)
		assert.False(t, ok)
	})

	t.Run("no options means no match", func(t *testing.T) {
		_, ok := matchOption(nil, usdc.Requirements)
		assert.False(t, ok)
	})
}

// TestAdvertisesEveryOption pins that a buyer sees every way it could pay, in the
// seller's order. A 402 that advertised only the first would make the accepts array
// decorative and leave a buyer holding the wrong asset with no route.
func TestAdvertisesEveryOption(t *testing.T) {
	gate := RequirePayment(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the gated handler must not run without a payment")
		}),
		PaymentConfig{Options: []PaymentOption{
			{FacilitatorURL: "http://a", Requirements: PaymentRequirements{
				Scheme: "exact", Network: "eip155:84532", Amount: "1000",
				Asset: "0xUSDC", PayTo: "0xSeller"}},
			{FacilitatorURL: "http://b", Requirements: PaymentRequirements{
				Scheme: "exact", Network: "gno:dev", Amount: "250000",
				Asset: "ugnot", PayTo: "g1seller"}},
		}},
	)

	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pixel", nil))

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	offer := decodePaymentRequiredHeader(t, rec.Header())

	require.Len(t, offer.Accepts, 2, "every option must be advertised")
	assert.Equal(t, "eip155:84532", offer.Accepts[0].Network, "seller order is preserved")
	assert.Equal(t, "gno:dev", offer.Accepts[1].Network)
	for i, entry := range offer.Accepts {
		assert.NotZero(t, entry.MaxTimeoutSeconds, "entry %d must carry a timeout, or a client reads it as expired", i)
	}
}

// TestRequirePayment_refusesAnOptionItCannotOffer pins that a seller answers its own
// misconfiguration rather than advertising an offer no payment could satisfy. One
// unofferable entry refuses the whole request: a partial offer set is a different
// product from the one the operator configured, and quietly dropping an entry hides
// the mistake.
func TestRequirePayment_refusesAnOptionItCannotOffer(t *testing.T) {
	good := PaymentOption{FacilitatorURL: "http://facilitator", Requirements: reqFixture()}

	for name, broken := range map[string]PaymentOption{
		"no facilitator to settle it": {Requirements: reqFixture()},
		"no amount": {FacilitatorURL: "http://facilitator", Requirements: func() PaymentRequirements {
			r := reqFixture()
			r.Amount = ""
			return r
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			gate := RequirePayment(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					t.Error("the gated handler must not run")
				}),
				PaymentConfig{Options: []PaymentOption{good, broken}},
			)
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pixel", nil))

			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"an unofferable option is the seller's fault, not a price to advertise")
			assert.Empty(t, rec.Header().Get(PaymentRequiredHeader),
				"nothing may be advertised when the offer set is broken")
		})
	}

	t.Run("no options at all", func(t *testing.T) {
		gate := RequirePayment(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("the gated handler must not run")
			}),
			PaymentConfig{},
		)
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pixel", nil))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// TestSettleRoutesToTheChosenFacilitator is the routing rule. Each option names its
// own facilitator because a facilitator serves one chain family; settling the USDC
// offer at the gno facilitator would ask a node that has never heard of Base to
// verify a Base payment.
//
// It also pins the security half: a client claiming an offer the seller never made
// must be refused WITHOUT any facilitator being called at all.
func TestSettleRoutesToTheChosenFacilitator(t *testing.T) {
	var hitA, hitB atomic.Int64
	var sawAmount atomic.Value
	facilitatorA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitA.Add(1)
		var req FacilitatorRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawAmount.Store(req.PaymentRequirements.Amount)
		writeJSON(w, SettleResponse{Success: true, Transaction: "abc", Payer: "0xBuyer"})
	}))
	defer facilitatorA.Close()
	facilitatorB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitB.Add(1)
		writeJSON(w, SettleResponse{Success: true, Transaction: "def"})
	}))
	defer facilitatorB.Close()

	usdc := PaymentRequirements{Scheme: "exact", Network: "eip155:84532",
		Amount: "1000", Asset: "0xUSDC", PayTo: "0xSeller", MaxTimeoutSeconds: 60}
	ugnot := PaymentRequirements{Scheme: "exact", Network: "gno:dev",
		Amount: "250000", Asset: "ugnot", PayTo: "g1seller", MaxTimeoutSeconds: 60}

	var served atomic.Int64
	gate := RequirePayment(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served.Add(1) }),
		PaymentConfig{Options: []PaymentOption{
			{FacilitatorURL: facilitatorA.URL, Requirements: usdc},
			{FacilitatorURL: facilitatorB.URL, Requirements: ugnot},
		}},
	)

	pay := func(accepted PaymentRequirements) *httptest.ResponseRecorder {
		header, err := EncodePaymentHeader(PaymentPayload{
			X402Version: 2, Accepted: accepted,
			Payload: SchemePayload{Transaction: "irrelevant-to-routing"},
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/pixel", nil)
		req.Header.Set(PaymentHeader, header)
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)
		return rec
	}

	t.Run("the chosen option decides the facilitator", func(t *testing.T) {
		rec := pay(usdc)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, int64(1), hitA.Load(), "the USDC facilitator settles the USDC offer")
		assert.Equal(t, int64(0), hitB.Load(), "the gno facilitator must not see it")
		assert.Equal(t, "1000", sawAmount.Load(), "settle carries the seller's own requirements")
	})

	t.Run("the other option routes the other way", func(t *testing.T) {
		rec := pay(ugnot)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, int64(1), hitB.Load(), "the gno facilitator settles the gno offer")
		assert.Equal(t, int64(1), hitA.Load(), "and the USDC one was not called again")
	})

	t.Run("an offer the seller never made is refused without settling", func(t *testing.T) {
		before := hitA.Load() + hitB.Load()
		cheap := usdc
		cheap.Amount = "1"
		rec := pay(cheap)
		assert.Equal(t, http.StatusPaymentRequired, rec.Code)
		assert.Equal(t, before, hitA.Load()+hitB.Load(), "no facilitator may be called")
		assert.Equal(t, int64(2), served.Load(), "the resource must not be served again")

		settle := decodeSettleHeader(t, rec.Header())
		assert.Equal(t, ReasonInvalidPayload, settle.ErrorReason,
			"the client must learn its claim was not one of the offers")
		assert.False(t, settle.Success)

		offer := decodePaymentRequiredHeader(t, rec.Header())
		require.Len(t, offer.Accepts, 2, "the refusal re-advertises what is actually on offer")
	})
}
