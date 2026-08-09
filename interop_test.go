package x402

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The interop fixture is a payment signed by @gnolang/gno-js-client, the
// official JS SDK, standing in for an x402 client that has never heard of gno.
// Its bytes are what such a client would put in the payload's transaction
// field: base64 of the SDK's own Tx encoding.
//
// The fixture is committed, so this test needs no JS toolchain. Regenerating it
// needs npm and is only for bumping the SDK or changing what the buyer signs.
//
// The expected payer is the well-known test1 address, derived from the mnemonic
// by the JS SDK and asserted against the value the Go side already trusts — so a
// divergence in key derivation or bech32 encoding fails rather than passes.
//
//go:generate ./testdata/gnojs/regen.sh
const (
	interopFixture = "gnojs_signed_send.b64"
	interopPayer   = "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5"
	interopPayTo   = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq"
	interopAmount  = "250000"
	interopMemo    = "x402-interop"
)

func interopRequirements() PaymentRequirements {
	return PaymentRequirements{
		Scheme:  "exact",
		Network: "gno:dev",
		Amount:  interopAmount,
		Asset:   "ugnot",
		PayTo:   interopPayTo,
		Extra:   map[string]any{"memo": interopMemo},
	}
}

func interopPayload(t *testing.T) SchemePayload {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", interopFixture))
	require.NoError(t, err, "interop fixture missing — regenerate with go generate ./x402")
	return SchemePayload{Transaction: strings.TrimSpace(string(raw))}
}

// TestVerifyStaticAcceptsJSSignedPayment is the cross-SDK wire check: a payment
// this package never encoded, produced by the JS SDK a foreign client would
// use, must decode and satisfy every static rule. It fails if the two SDKs
// disagree on the tx encoding, the message's registered type name, address
// derivation, or coin representation — the whole surface a foreign buyer
// touches, and the one thing no amount of Go-side fixture testing can prove.
func TestVerifyStaticAcceptsJSSignedPayment(t *testing.T) {
	tx, payer, reason := VerifyStatic(interopRequirements(), interopPayload(t))

	require.Empty(t, reason, "JS-signed payment refused")
	require.NotNil(t, tx)
	assert.Equal(t, interopPayer, payer, "payer address disagrees across SDKs")

	require.Len(t, tx.Msgs, 1)
	send, ok := tx.Msgs[0].(bank.MsgSend)
	require.True(t, ok, "decoded message is %T, want bank.MsgSend", tx.Msgs[0])
	assert.Equal(t, interopPayer, send.FromAddress.String())
	assert.Equal(t, interopPayTo, send.ToAddress.String())
	assert.Equal(t, interopAmount+"ugnot", send.Amount.String())
	assert.Equal(t, interopMemo, tx.Memo, "memo binding lost in the JS encoding")
	require.Len(t, tx.Signatures, 1)
	assert.NotEmpty(t, tx.Signatures[0].Signature, "signature absent")
	assert.NotNil(t, tx.Signatures[0].PubKey, "public key absent, so no account can verify the signature")
}
