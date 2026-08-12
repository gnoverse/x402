package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

// The seller registers this with the canonical middleware, which accepts nothing
// else, so a drifted signature must fail to build rather than at a seller's boot.
var _ x402.SchemeNetworkServer = (*ExactGnoScheme)(nil)

func TestSchemeIsExact(t *testing.T) {
	assert.Equal(t, "exact", NewExactGnoScheme().Scheme())
}

// A seller writing Go hands ParsePrice the struct, because the middleware passes
// PaymentOption.Price through untouched.
func TestParsePriceAcceptsAnAssetAmountStruct(t *testing.T) {
	got, err := NewExactGnoScheme().ParsePrice(
		x402.AssetAmount{Asset: "ugnot", Amount: "250000"}, "gno:dev")
	require.NoError(t, err)
	assert.Equal(t, x402.AssetAmount{Asset: "ugnot", Amount: "250000"}, got)
}

// Route configuration that arrived as a JSON document carries the price as a map,
// so one price must resolve the same whether it was written or decoded.
func TestParsePriceAcceptsAnAssetAmountMap(t *testing.T) {
	got, err := NewExactGnoScheme().ParsePrice(
		map[string]interface{}{"asset": "ugnot", "amount": "250000"}, "gno:dev")
	require.NoError(t, err)
	assert.Equal(t, x402.AssetAmount{Asset: "ugnot", Amount: "250000"}, got)
}

func TestParsePriceKeepsTheAssetsOwnExtra(t *testing.T) {
	got, err := NewExactGnoScheme().ParsePrice(
		x402.AssetAmount{Asset: "ugnot", Amount: "1", Extra: map[string]interface{}{"note": "keep me"}},
		"gno:dev")
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"note": "keep me"}, got.Extra)
}

// gno has no default stablecoin, so a dollar amount names no asset. Resolving one
// silently would price the resource in a token nobody agreed on.
func TestParsePriceRejectsADollarPrice(t *testing.T) {
	_, err := NewExactGnoScheme().ParsePrice("$0.001", "gno:dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AssetAmount",
		"the refusal has to tell the seller what to write instead")
}

func TestParsePriceRejectsAnUnpayableAmount(t *testing.T) {
	for name, price := range map[string]x402.Price{
		"no asset":        x402.AssetAmount{Amount: "1"},
		"no amount":       x402.AssetAmount{Asset: "ugnot"},
		"zero":            x402.AssetAmount{Asset: "ugnot", Amount: "0"},
		"negative":        x402.AssetAmount{Asset: "ugnot", Amount: "-1"},
		"not a number":    x402.AssetAmount{Asset: "ugnot", Amount: "some"},
		"amount as float": map[string]interface{}{"asset": "ugnot", "amount": 250000.0},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewExactGnoScheme().ParsePrice(price, "gno:dev")
			assert.Error(t, err)
		})
	}
}

// TestParsePriceRejectsAPriceTheChainCannotParse covers the prices a whole-number
// check accepts and the chain's coin grammar does not.
//
// A price that reaches the requirements is the string the buyer's payment is
// matched against, and the facilitator parses it with std.ParseCoins. Anything
// that parse refuses publishes a route no payment can satisfy — and the refusal
// arrives as an amount mismatch, which blames the payer for the seller's typo. So
// the seller's own grammar has to be the chain's.
func TestParsePriceRejectsAPriceTheChainCannotParse(t *testing.T) {
	for name, price := range map[string]x402.Price{
		// The denomination is lower-case only.
		"upper-case denomination": x402.AssetAmount{Asset: "UGNOT", Amount: "250000"},
		// And at least three characters long.
		"denomination too short": x402.AssetAmount{Asset: "gn", Amount: "250000"},
		// The amount is digits, with no sign — which ParseInt accepts.
		"signed amount": x402.AssetAmount{Asset: "ugnot", Amount: "+250000"},
		// A denomination holding a space cannot round-trip through one coin string.
		"denomination with a space": x402.AssetAmount{Asset: "u gnot", Amount: "250000"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewExactGnoScheme().ParsePrice(price, "gno:dev")
			assert.Error(t, err)
		})
	}
}

// ugnot is indivisible, so a fractional amount is not a small payment — it is a
// price the chain cannot charge.
func TestParsePriceRejectsAFractionalAmount(t *testing.T) {
	_, err := NewExactGnoScheme().ParsePrice(
		x402.AssetAmount{Asset: "ugnot", Amount: "0.5"}, "gno:dev")
	assert.Error(t, err)
}

func enhance(t *testing.T, req types.PaymentRequirements) types.PaymentRequirements {
	t.Helper()
	got, err := NewExactGnoScheme().EnhancePaymentRequirements(
		context.Background(), req, types.SupportedKind{}, nil)
	require.NoError(t, err)
	return got
}

// The payer pays the network fee inside the transaction they sign and the
// facilitator holds no key. A buyer can only learn that from the requirements,
// and XRPL — the shipped mechanism with gno's push model — makes the flag
// mandatory, so a gno seller never hand-writes it.
func TestEnhanceDeclaresTheFeeIsNotSponsored(t *testing.T) {
	got := enhance(t, types.PaymentRequirements{Scheme: "exact", Network: "gno:dev"})
	assert.Equal(t, false, got.Extra["areFeesSponsored"])
}

// authorization is the specification's default flow and may be omitted, so
// emitting the key would advertise a deviation we are not making.
func TestEnhanceEmitsNoPaymentFlowKey(t *testing.T) {
	got := enhance(t, types.PaymentRequirements{Scheme: "exact", Network: "gno:dev"})
	_, declared := got.Extra["paymentFlow"]
	assert.False(t, declared)
}

// The middleware merges a route's own extra before enhancement, so a memo the
// seller set to bind an invoice has to survive.
func TestEnhanceKeepsASellersMemo(t *testing.T) {
	got := enhance(t, types.PaymentRequirements{
		Scheme:  "exact",
		Network: "gno:dev",
		Extra:   map[string]interface{}{"memo": "invoice-7"},
	})
	assert.Equal(t, "invoice-7", got.Extra["memo"])
	assert.Equal(t, false, got.Extra["areFeesSponsored"])
}
