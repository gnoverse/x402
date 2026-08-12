package server_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"
	"github.com/x402-foundation/x402/go/v2/types"

	gnoexact "github.com/gnoverse/x402/mechanisms/gno/exact/server"
)

const (
	testNetwork = "gno:test14"
	testPayTo   = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq"
)

// recordingFacilitator answers the facilitator API without a chain and records
// the order it was called in, alongside the resource itself.
//
// What it verifies is deliberately not the subject: these tests are about when
// the middleware verifies and when it settles. A payment the chain agrees with is
// what the facilitator's own tests and the integration tests are for.
type recordingFacilitator struct {
	mu    sync.Mutex
	calls []string
}

func (f *recordingFacilitator) record(step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, step)
}

func (f *recordingFacilitator) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *recordingFacilitator) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /supported", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"kinds":      []map[string]any{{"x402Version": 2, "scheme": "exact", "network": testNetwork}},
			"extensions": []string{},
			"signers":    map[string][]string{},
		})
	})

	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, _ *http.Request) {
		f.record("verify")
		writeJSON(w, map[string]any{"isValid": true, "payer": "g1payer"})
	})

	mux.HandleFunc("POST /settle", func(w http.ResponseWriter, _ *http.Request) {
		f.record("settle")
		writeJSON(w, map[string]any{
			"success": true, "transaction": "deadbeef", "network": testNetwork, "payer": "g1payer",
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// weatherSeller guards one resource with the canonical middleware and the gno
// mechanism. It deliberately repeats the route rather than sharing one with the
// canonical snippet test, whose value is that it reads exactly as documented.
func weatherSeller(facilitatorURL string, resource http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /weather", resource)

	return nethttpmw.X402Payment(nethttpmw.Config{
		Routes: x402http.RoutesConfig{
			"GET /weather": {
				Accepts: x402http.PaymentOptions{{
					Scheme:  "exact",
					PayTo:   testPayTo,
					Price:   x402.AssetAmount{Asset: "ugnot", Amount: "250000"},
					Network: testNetwork,
				}},
			},
		},
		Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitatorURL}),
		Schemes: []nethttpmw.SchemeConfig{
			{Network: testNetwork, Server: gnoexact.NewExactGnoScheme()},
		},
		SyncFacilitatorOnStart: true,
	})(mux)
}

// buy makes the two requests a buyer makes: the unpaid one that returns the terms,
// then the same request carrying a payload built from exactly those terms.
func buy(t *testing.T, sellerURL string) *http.Response {
	t.Helper()

	unpaid, err := http.Get(sellerURL + "/weather")
	require.NoError(t, err)
	require.NoError(t, unpaid.Body.Close())
	require.Equal(t, http.StatusPaymentRequired, unpaid.StatusCode)

	terms, err := base64.StdEncoding.DecodeString(unpaid.Header.Get("PAYMENT-REQUIRED"))
	require.NoError(t, err)

	var required x402.PaymentRequired
	require.NoError(t, json.Unmarshal(terms, &required))
	require.Len(t, required.Accepts, 1)

	payload, err := json.Marshal(types.PaymentPayload{
		X402Version: 2,
		Accepted:    required.Accepts[0],
		Payload:     map[string]any{"transaction": "a recording facilitator reads no transaction"},
	})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, sellerURL+"/weather", nil)
	require.NoError(t, err)
	req.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(payload))

	paid, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = paid.Body.Close() })
	return paid
}

// The ordering is authorization, the specification's default: the payment is
// checked before the resource runs and taken only once it has. Inheriting that
// from the ecosystem's middleware is the reason to use it, so it is asserted
// rather than assumed.
func TestThePaymentIsTakenAfterTheResourceRuns(t *testing.T) {
	facilitator := &recordingFacilitator{}
	facilitatorServer := httptest.NewServer(facilitator.handler())
	t.Cleanup(facilitatorServer.Close)

	seller := httptest.NewServer(weatherSeller(facilitatorServer.URL,
		func(w http.ResponseWriter, _ *http.Request) {
			facilitator.record("resource")
			_, _ = w.Write([]byte(`{"forecast":"sunny"}`))
		}))
	t.Cleanup(seller.Close)

	paid := buy(t, seller.URL)

	assert.Equal(t, http.StatusOK, paid.StatusCode)
	assert.Equal(t, []string{"verify", "resource", "settle"}, facilitator.steps())
}

// The exposure authorization accepts is the seller's, and it is bounded by this:
// a resource that failed is never charged for. Without it the buyer would pay for
// an error, which is the one loss the protocol gives them no way to undo.
func TestAResourceThatFailedIsNotCharged(t *testing.T) {
	facilitator := &recordingFacilitator{}
	facilitatorServer := httptest.NewServer(facilitator.handler())
	t.Cleanup(facilitatorServer.Close)

	seller := httptest.NewServer(weatherSeller(facilitatorServer.URL,
		func(w http.ResponseWriter, _ *http.Request) {
			facilitator.record("resource")
			http.Error(w, "the forecast is unavailable", http.StatusInternalServerError)
		}))
	t.Cleanup(seller.Close)

	paid := buy(t, seller.URL)

	assert.Equal(t, http.StatusInternalServerError, paid.StatusCode)
	assert.Equal(t, []string{"verify", "resource"}, facilitator.steps())
	assert.NotContains(t, facilitator.steps(), "settle",
		"a buyer must not be charged for a response the seller could not produce")
}
