package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"

	x402gno "github.com/gnoverse/x402"
	gnoexact "github.com/gnoverse/x402/mechanisms/gno/exact/server"
)

// keylessNode stands in for the chain. The 402 is decided from the seller's own
// configuration, so a request that never carries a payment reaches no chain, and
// a method called here means the middleware went somewhere it should not.
type keylessNode struct{}

func (keylessNode) SignerAccount(context.Context, *std.Tx) (x402gno.SignerAccount, error) {
	panic("an unpaid request must be answered without reading an account")
}

func (keylessNode) Simulate(*std.Tx) error {
	panic("an unpaid request must be answered without simulating")
}

func (keylessNode) Broadcast(*std.Tx) (string, int64, error) {
	panic("an unpaid request must be answered without broadcasting")
}

// TestTheCanonicalSellerSnippetOffersGno is the claim this mechanism exists to
// make: the seller code from x402.org's front page, with gno listed as a way to
// pay, offers a gno payment for an ordinary HTTP resource.
//
// The resource is weather data. Nothing here knows about realms or contract
// calls, and nothing gno-specific appears outside the two network strings and the
// registered scheme — everything else is the ecosystem's own middleware.
func TestTheCanonicalSellerSnippetOffersGno(t *testing.T) {
	// A facilitator has to be answering /supported before the middleware is built:
	// the middleware syncs during construction and only warns when it cannot, so a
	// facilitator started afterwards would leave the route unable to price itself.
	facilitator := httptest.NewServer(x402gno.NewFacilitator(keylessNode{}, "test14").Handler())
	t.Cleanup(facilitator.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/weather", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"forecast":"sunny"}`))
	})

	routes := x402http.RoutesConfig{
		"GET /weather": {
			Accepts: x402http.PaymentOptions{{
				Scheme:  "exact",
				PayTo:   "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
				Price:   x402.AssetAmount{Asset: "ugnot", Amount: "250000"},
				Network: "gno:test14",
			}},
			Description: "Weather data",
			MimeType:    "application/json",
		},
	}

	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes:      routes,
		Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitator.URL}),
		Schemes: []nethttpmw.SchemeConfig{
			{Network: "gno:test14", Server: gnoexact.NewExactGnoScheme()},
		},
		SyncFacilitatorOnStart: true,
	})(mux)

	seller := httptest.NewServer(handler)
	t.Cleanup(seller.Close)

	resp, err := http.Get(seller.URL + "/weather")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusPaymentRequired, resp.StatusCode,
		"an unpaid request for a priced route is answered with the protocol's own status")

	// v2 carries the requirements in the PAYMENT-REQUIRED header, base64 JSON. The
	// body is the seller's own, and defaults to an empty JSON object for a client
	// that asked for anything but HTML.
	header := resp.Header.Get("PAYMENT-REQUIRED")
	require.NotEmpty(t, header, "the 402 has to say what would pay for the resource")
	body, err := base64.StdEncoding.DecodeString(header)
	require.NoError(t, err)

	var required struct {
		X402Version int    `json:"x402Version"`
		Error       string `json:"error"`
		Accepts     []struct {
			Scheme            string         `json:"scheme"`
			Network           string         `json:"network"`
			Asset             string         `json:"asset"`
			Amount            string         `json:"amount"`
			PayTo             string         `json:"payTo"`
			MaxTimeoutSeconds int            `json:"maxTimeoutSeconds"`
			Extra             map[string]any `json:"extra"`
		} `json:"accepts"`
	}
	require.NoError(t, json.Unmarshal(body, &required))

	assert.Equal(t, 2, required.X402Version)
	require.Len(t, required.Accepts, 1,
		"the route offers exactly the one way to pay it listed, but answered %s", body)

	offer := required.Accepts[0]
	assert.Equal(t, "exact", offer.Scheme)
	assert.Equal(t, "gno:test14", offer.Network)
	assert.Equal(t, "ugnot", offer.Asset, "the price is quoted in the denomination the seller named")
	assert.Equal(t, "250000", offer.Amount)
	assert.Equal(t, "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq", offer.PayTo)

	// The mechanism ran: only EnhancePaymentRequirements sets this, so its presence
	// proves the seller's registration took effect rather than the middleware
	// having priced the route without ever consulting gno.
	assert.Equal(t, false, offer.Extra["areFeesSponsored"],
		"the payer pays the network fee inside the transaction they sign")

	_, declaresFlow := offer.Extra["paymentFlow"]
	assert.False(t, declaresFlow, "authorization is the default flow and is omitted")
}
