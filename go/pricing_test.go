package x402

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quantityPricing prices a request from its ?n= quantity, the way a seller
// charging per item in a batch would. No quantity means no price, which it says by
// offering nothing rather than by offering something unpayable.
func quantityPricing(unit int64, facilitatorURL string) func(*http.Request) []PaymentOption {
	return func(r *http.Request) []PaymentOption {
		n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		if err != nil || n <= 0 {
			return nil
		}
		req := reqFixture()
		req.Amount = strconv.FormatInt(unit*n, 10)
		return []PaymentOption{{FacilitatorURL: facilitatorURL, Requirements: req}}
	}
}

// A resource whose price depends on how much was asked for cannot advertise a
// price fixed at wiring time.
func TestRequirePayment_PricesEachRequestFromItsContent(t *testing.T) {
	cfg := PaymentConfig{OptionsFor: quantityPricing(250000, "http://unused.invalid")}
	handler := RequirePayment(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("an unpaid request must not reach the resource")
	}), cfg)

	for _, tc := range []struct{ n, want string }{
		{"1", "250000"},
		{"8", "2000000"},
		{"40", "10000000"},
	} {
		t.Run("n="+tc.n, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pixels?n="+tc.n, nil))

			require.Equal(t, http.StatusPaymentRequired, rec.Code)
			got := decodePaymentRequiredHeader(t, rec.Header())
			require.Len(t, got.Accepts, 1)
			assert.Equal(t, tc.want, got.Accepts[0].Amount,
				"the 402 must quote this request's price, not the wiring-time one")

			var body PaymentRequired
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.Accepts, 1)
			assert.Equal(t, tc.want, body.Accepts[0].Amount, "the body must agree with the header")
		})
	}
}

// The priced requirements live for one request. Sharing them would let a cheap
// request quote an expensive one's price — or worse, settle at it.
func TestRequirePayment_PriceDoesNotLeakBetweenConcurrentRequests(t *testing.T) {
	cfg := PaymentConfig{OptionsFor: quantityPricing(1000, "http://unused.invalid")}
	handler := RequirePayment(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg)

	var wg sync.WaitGroup
	for n := 1; n <= 24; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				fmt.Sprintf("/pixels?n=%d", n), nil))
			got := decodePaymentRequiredHeader(t, rec.Header())
			require.Len(t, got.Accepts, 1)
			assert.Equal(t, strconv.Itoa(n*1000), got.Accepts[0].Amount,
				"request for n=%d was quoted another request's price", n)
		}(n)
	}
	wg.Wait()
}

// The facilitator must be asked to settle against the price this request was
// quoted. Settling against the wiring-time requirements would accept a
// single-pixel payment for a forty-pixel order.
func TestRequirePayment_SettlesAgainstThePricedRequirements(t *testing.T) {
	var seen FacilitatorRequest
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &seen))
		require.NoError(t, json.NewEncoder(w).Encode(SettleResponse{
			Success: true, Network: "gno:dev", Payer: "g1payer", Transaction: "deadbeef",
		}))
	}))
	defer facilitator.Close()

	cfg := PaymentConfig{OptionsFor: quantityPricing(250000, facilitator.URL)}
	var served bool
	handler := RequirePayment(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served = true
	}), cfg)

	// The claim is the offer this request was quoted, which is what a client echoes
	// back: it accepts an entry out of the 402 it just received. Claiming the
	// wiring-time price instead names an offer this request never made.
	quoted := reqFixture()
	quoted.Amount = "2000000"

	req := httptest.NewRequest(http.MethodPost, "/pixels?n=8", nil)
	req.Header.Set(PaymentHeader, paymentHeader(t, quoted))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, served, "a settled payment must be served")
	assert.Equal(t, "2000000", seen.PaymentRequirements.Amount,
		"the settle call must carry this request's price")
}

// A pricing function that cannot price the request leaves the seller with no
// price to state. Advertising a 402 anyway would invite a payment against
// requirements the seller never meant, so it refuses without advertising.
func TestRequirePayment_UnpriceableRequestIsRefusedWithoutAdvertising(t *testing.T) {
	cfg := PaymentConfig{OptionsFor: quantityPricing(250000, "http://unused.invalid")}
	handler := RequirePayment(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("an unpriceable request must not reach the resource")
	}), cfg)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pixels?n=0", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotEqual(t, http.StatusPaymentRequired, rec.Code,
		"a price the seller could not compute must not be advertised as a 402")
	assert.Empty(t, rec.Header().Get(PaymentRequiredHeader),
		"no requirements may be advertised when none could be computed")
}

// With no pricing function the wiring-time requirements still price every
// request, so a flat-priced resource needs no hook.
func TestRequirePayment_WithoutPricingUsesTheConfiguredRequirements(t *testing.T) {
	cfg := oneOptionConfig("http://unused.invalid", reqFixture())
	handler := RequirePayment(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pixels?n=8", nil))

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	got := decodePaymentRequiredHeader(t, rec.Header())
	require.Len(t, got.Accepts, 1)
	assert.Equal(t, reqFixture().Amount, got.Accepts[0].Amount)
}
