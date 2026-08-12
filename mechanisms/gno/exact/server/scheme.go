// Package server lets a seller list gno as a way to pay for any HTTP resource,
// by registering the gno "exact" mechanism with the canonical x402 middleware.
//
// The resource is any endpoint and the chain is only the payment rail, so nothing
// here knows about realms or contract calls.
package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

// ExactGnoScheme is the seller half of the gno "exact" mechanism. It resolves a
// price into a gno denomination and states the payment terms that only gno knows,
// so a seller writes neither.
type ExactGnoScheme struct{}

// NewExactGnoScheme returns the scheme a seller registers for a gno network.
func NewExactGnoScheme() *ExactGnoScheme { return &ExactGnoScheme{} }

// Scheme names the x402 payment scheme this implements.
func (s *ExactGnoScheme) Scheme() string { return "exact" }

// ParsePrice resolves a route's price into the gno denomination and amount the
// requirements will publish.
//
// gno has no default asset, so unlike chains with a canonical stablecoin there is
// nothing a bare number could mean. A price therefore names its denomination, and
// a dollar amount is refused rather than resolved into a token nobody agreed on.
//
// The middleware passes the seller's price through untouched, so it arrives as an
// AssetAmount when the route was written in Go and as a map when the route was
// decoded from a document. Both are the same price and both resolve here.
func (s *ExactGnoScheme) ParsePrice(price x402.Price, _ x402.Network) (x402.AssetAmount, error) {
	var amount x402.AssetAmount

	switch p := price.(type) {
	case x402.AssetAmount:
		amount = p
	case map[string]interface{}:
		var err error
		if amount, err = assetAmountFromMap(p); err != nil {
			return x402.AssetAmount{}, err
		}
	default:
		return x402.AssetAmount{}, fmt.Errorf(
			"x402: gno price is %T, want an x402.AssetAmount naming the denomination, "+
				"such as {Asset: \"ugnot\", Amount: \"250000\"}: gno has no default asset, "+
				"so a bare or dollar amount names nothing to charge", price)
	}

	if err := checkPayable(amount); err != nil {
		return x402.AssetAmount{}, err
	}
	return amount, nil
}

// assetAmountFromMap reads a price that reached the route config as decoded JSON.
func assetAmountFromMap(m map[string]interface{}) (x402.AssetAmount, error) {
	raw, present := m["amount"]
	if !present {
		return x402.AssetAmount{}, errors.New("x402: gno price names no amount")
	}
	amount, ok := raw.(string)
	if !ok {
		return x402.AssetAmount{}, fmt.Errorf(
			"x402: gno price amount is %T, want a string: a JSON number cannot carry "+
				"the chain's whole range of amounts exactly", raw)
	}

	asset, _ := m["asset"].(string)
	extra, _ := m["extra"].(map[string]interface{})
	return x402.AssetAmount{Asset: asset, Amount: amount, Extra: extra}, nil
}

// checkPayable refuses a price the chain could not charge. Amounts are whole
// numbers of the denomination's smallest unit, which is what a gno coin holds, so
// a fractional or out-of-range amount is not a small price but an unchargeable one.
func checkPayable(amount x402.AssetAmount) error {
	if amount.Asset == "" {
		return errors.New(
			"x402: gno price names no asset; a gno price names its denomination, such as \"ugnot\"")
	}
	if amount.Amount == "" {
		return errors.New("x402: gno price names no amount")
	}

	units, err := strconv.ParseInt(amount.Amount, 10, 64)
	if err != nil {
		return fmt.Errorf("x402: gno price %q is not a whole number of %s: %w",
			amount.Amount, amount.Asset, err)
	}
	if units <= 0 {
		return fmt.Errorf("x402: gno price %q %s is not something a buyer can pay",
			amount.Amount, amount.Asset)
	}
	return nil
}

// EnhancePaymentRequirements states the gno terms a buyer cannot infer.
//
// The payer pays the network fee inside the transaction they sign, and the
// facilitator holds no key and only broadcasts. A buyer can learn that only from
// the requirements, so the flag is declared here rather than left to each seller.
//
// No paymentFlow key is emitted: the ordering is authorization, the
// specification's default, which may be omitted.
func (s *ExactGnoScheme) EnhancePaymentRequirements(
	_ context.Context,
	requirements types.PaymentRequirements,
	_ types.SupportedKind,
	_ []string,
) (types.PaymentRequirements, error) {
	if requirements.Extra == nil {
		requirements.Extra = make(map[string]interface{}, 1)
	}
	requirements.Extra["areFeesSponsored"] = false
	return requirements, nil
}
