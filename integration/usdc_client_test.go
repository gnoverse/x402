//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"

	"github.com/gnoverse/x402"
)

// The USDC option this seller advertises, matching cmd/gnowars. The extra fields are
// the EIP-712 domain the authorization is signed over, and the ABSENCE of
// extra.assetTransferMethod is what selects the EIP-3009 path — the one where the buyer
// signs an authorization instead of a transaction and therefore needs no ETH.
const (
	usdcNetwork = "eip155:84532"
	usdcAsset   = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
)

func usdcRequirements(payTo, amount string) x402.PaymentRequirements {
	return x402.PaymentRequirements{
		Scheme:            "exact",
		Network:           usdcNetwork,
		Amount:            amount,
		Asset:             usdcAsset,
		PayTo:             payTo,
		MaxTimeoutSeconds: 60,
		Extra:             map[string]any{"name": "USDC", "version": "2"},
	}
}

// buyerDir holds the JS buyers. buy-usdc.mjs runs from there so it resolves the
// scoped @x402/* packages `make js` installs.
const buyerDir = "../js"

// paidResource is what the gated handler serves once a payment settles.
const paidResource = `{"painted":true}`

// usdcClientReport is what buy-usdc.mjs prints on stdout.
type usdcClientReport struct {
	Status      int    `json:"status"`
	Body        string `json:"body"`
	Buyer       string `json:"buyer"`
	Payout      string `json:"payout"`
	Payer       string `json:"payer"`
	Network     string `json:"network"`
	Transaction string `json:"transaction"`
}

// throwawayEVMKey is a fresh secp256k1 scalar, generated per run and never persisted.
// It signs an authorization that this test never submits anywhere, and holding no key
// material in the repository keeps it that way by construction.
func throwawayEVMKey(t *testing.T) string {
	t.Helper()
	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)
	// The top byte is forced below the curve order's leading byte and away from zero, so
	// the value is a valid scalar without implementing the full range check.
	key[0] = 0x01
	return "0x" + hex.EncodeToString(key[:])
}

// runUsdcClient runs the USDC buyer against a seller and returns its report. It skips
// loudly rather than failing when the JS toolchain is absent, like its gno sibling.
func runUsdcClient(t *testing.T, sellerURL, payout string) usdcClientReport {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed — skipping the USDC client; install node and run `make js` to include it")
	}
	dir, err := filepath.Abs(buyerDir)
	require.NoError(t, err)
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		t.Skip("JS buyer dependencies not installed — run `make js` to include the USDC client")
	}

	cmd := exec.CommandContext(context.Background(), node, "buy-usdc.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"X402_SELLER_URL="+sellerURL,
		"GNO_PAYOUT_ADDRESS="+payout,
		"EVM_PRIVATE_KEY="+throwawayEVMKey(t),
	)
	var stderr []byte
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		stderr = ee.Stderr
	}
	require.NoError(t, err, "USDC client failed\nstdout:\n%s\nstderr:\n%s", out, stderr)

	var report usdcClientReport
	require.NoError(t, json.Unmarshal(out, &report), "USDC client printed no report: %s", out)
	return report
}

// TestX402_usdcClientPaysThroughSeller is the USDC half of the interop claim, proved
// without money and without leaving this machine.
//
// A stock @x402/evm client — which has never heard of gno — is refused by the real
// seller middleware, reads the USDC entry out of the 402, signs an EIP-3009
// authorization for it, and is served. The facilitator is a local stub: nothing settles,
// no funds exist, and no request reaches the internet.
//
// What that establishes is the one thing a real /settle cannot tell apart from an empty
// balance: that our accepts[] entry is shaped so a stock client will sign it at all. A
// wrong domain, a missing extra field or an amount it cannot parse would fail here, in a
// test, instead of after somebody spends a faucet drip.
func TestX402_usdcClientPaysThroughSeller(t *testing.T) {
	const (
		sellerUSDCAddress = "0x1111111111111111111111111111111111111111"
		amount            = "600000" // 0.6 USDC in atomic units
		payout            = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq"
	)
	requirements := usdcRequirements(sellerUSDCAddress, amount)

	// The stub records the settle request as RAW JSON. Decoding it through this
	// module's own types would lose any field they do not declare — which is exactly
	// the failure this test exists to detect, so it must not share their view.
	var settles atomic.Int64
	var settleBody atomic.Value
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settles.Add(1)
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		settleBody.Store(raw)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(x402.SettleResponse{
			Success: true, Transaction: "0xstubbed", Network: usdcNetwork,
			Payer: "0xBuyer",
		}))
	}))
	t.Cleanup(facilitator.Close)

	// The Confirmer is set because cmd/gnowars always sets one, and omitting it here is
	// what let a seller ship that could not take a USDC payment at all: the gno chain view
	// ran gno's own VerifyStatic over an EIP-3009 payload and refused every one. A gno node
	// this test never reaches is the right stand-in — if the confirmer is consulted about a
	// Base payment, dialing it fails and the request cannot succeed.
	unreachable, err := rpcclient.NewHTTPClient("http://127.0.0.1:1")
	require.NoError(t, err)

	var served atomic.Int64
	gate := x402.RequirePayment(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, paidResource)
		}),
		x402.PaymentConfig{
			Options: []x402.PaymentOption{
				{FacilitatorURL: facilitator.URL, Requirements: requirements},
			},
			Confirmer: x402.NewGnoclientNode(&gnoclient.Client{RPCClient: unreachable}),
		},
	)

	// The client's own PAYMENT-SIGNATURE and request body, captured before the middleware
	// reads them, so what the facilitator received can be compared against what was
	// actually sent — and so the payout is read off the wire rather than out of the
	// environment variable this test set.
	var sentHeader, sentBody atomic.Value
	seller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get(x402.PaymentHeader); h != "" {
			sentHeader.Store(h)
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			sentBody.Store(raw)
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		gate.ServeHTTP(w, r)
	}))
	t.Cleanup(seller.Close)

	report := runUsdcClient(t, seller.URL, payout)

	require.Equal(t, http.StatusOK, report.Status,
		"a stock EVM client could not pay the advertised USDC offer: %s", report.Body)
	assert.JSONEq(t, paidResource, report.Body)
	assert.Equal(t, int64(1), served.Load(), "the resource must be served exactly once")
	assert.Equal(t, int64(1), settles.Load(), "the USDC offer must settle at the EVM facilitator")

	// Off the wire, not out of the environment: a buyer that dropped the payout would
	// still print it in its own report, and the seller would then have nothing to credit.
	rawBody, ok := sentBody.Load().([]byte)
	require.True(t, ok, "the paid request carried no body")
	var order struct {
		Payout string `json:"payout"`
	}
	require.NoError(t, json.Unmarshal(rawBody, &order))
	assert.Equal(t, payout, order.Payout,
		"the paid request must carry the gno address the canvas credits")

	sent := decodeSentPayload(t, sentHeader)
	got := decodeSettleRequest(t, settleBody)

	// The seller sends its OWN requirements, never the client's echo of them.
	require.Equal(t, requirements.Amount, got.PaymentRequirements.Amount)
	require.Equal(t, requirements.PayTo, got.PaymentRequirements.PayTo)
	require.Equal(t, requirements.Asset, got.PaymentRequirements.Asset)
	require.Equal(t, requirements.Network, got.PaymentRequirements.Network)
	assert.Equal(t, requirements.Extra, got.PaymentRequirements.Extra,
		"the EIP-712 domain the authorization was signed over must reach the facilitator")

	// And it forwards the scheme payload untouched. This implementation's own scheme
	// carries a signed transaction; EVM's carries an authorization and a signature.
	// A seller that re-encoded the payload through its own scheme's shape would hand
	// the facilitator an empty one and settle nothing, while reporting success.
	assert.Equal(t, sent["payload"], got.PaymentPayload["payload"],
		"the scheme payload must reach the facilitator exactly as the client signed it")
	assert.NotEmpty(t, got.PaymentPayload["payload"], "the payload must not arrive empty")
	assert.Equal(t, sent["accepted"], got.PaymentPayload["accepted"],
		"the accepted object the client signed against must survive the hop")
}

// decodeSentPayload decodes the PAYMENT-SIGNATURE the client sent, as raw JSON.
func decodeSentPayload(t *testing.T, header atomic.Value) map[string]any {
	t.Helper()
	raw, ok := header.Load().(string)
	require.True(t, ok, "the client sent no %s header", x402.PaymentHeader)
	decoded, err := base64.StdEncoding.DecodeString(raw)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(decoded, &payload))
	return payload
}

// settleRequest is the facilitator's view of a settle call, with the payment payload
// left as raw JSON so a field this module does not declare is still visible.
type settleRequest struct {
	PaymentPayload      map[string]any           `json:"paymentPayload"`
	PaymentRequirements x402.PaymentRequirements `json:"paymentRequirements"`
}

func decodeSettleRequest(t *testing.T, body atomic.Value) settleRequest {
	t.Helper()
	raw, ok := body.Load().([]byte)
	require.True(t, ok, "the facilitator received no settle request")
	var got settleRequest
	require.NoError(t, json.Unmarshal(raw, &got), "settle request was not JSON: %s", raw)
	return got
}
