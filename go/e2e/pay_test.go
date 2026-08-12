// Package e2e joins the halves. Every other test in this repo proves one part —
// the seller emits a conformant 402, the ordering is authorization, a JS-signed
// payment satisfies the static rules, the buyer's sign doc matches the wallet's.
// None of them can say a payment happened, because that needs a chain.
//
// This drives gno.land's txtar harness, which starts a real in-memory node, and
// adds two commands to it: one that stands up the facilitator against that node,
// one that guards an HTTP resource with the canonical middleware. The buyer is the
// JS one, run by `exec node`, so the assertion crosses both languages.
package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/gno.land/pkg/integration"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"

	x402gno "github.com/gnoverse/x402/go"
	gnoexact "github.com/gnoverse/x402/go/mechanisms/gno/exact/server"
)

// The harness's in-memory node serves this chain, so it is the CAIP-2 reference
// every offer and every signature in the script is bound to.
const (
	chainID = "tendermint_test"
	network = "gno:" + chainID

	priceAmount = "250000"
	priceAsset  = "ugnot"
)

func TestAStockClientPaysAGnoSeller(t *testing.T) {
	buyer := buyerScript(t)

	params := testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			env.Setenv("X402_BUYER", buyer)
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"x402facilitator": facilitatorCmd,
			"x402seller":      sellerCmd,
		},
	}

	// Wraps our Setup and merges our Cmds rather than replacing either.
	require.NoError(t, integration.SetupGnolandTestscript(t, &params))
	testscript.Run(t, params)
}

// buyerScript locates the JS buyer, two levels up: this module is go/e2e and the
// JS tree is the repository's own js/, a sibling of go/.
//
// A missing buy.mjs FAILS rather than skips. It is committed, so its absence means
// the path is wrong — which is how a repository reshuffle turns this whole test
// into a silent pass. Only the built output is allowed to be absent, because
// building it needs npm and a Go-only checkout should not be blocked on that.
func buyerScript(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	repo := filepath.Join(wd, "..", "..")

	buyer := filepath.Join(repo, "js", "buy.mjs")
	_, err = os.Stat(buyer)
	require.NoError(t, err, "the JS buyer is committed, so this path is wrong")

	// buy.mjs imports the mechanism by package name, so the package has to be
	// installed and built, not merely present in the tree.
	if _, err := os.Stat(filepath.Join(repo, "js", "dist", "mechanism.mjs")); err != nil {
		t.Skipf("the JS mechanism is not built (%v); run `make js`", err)
	}
	return buyer
}

// x402facilitator stands up the facilitator against the running node. It holds no
// key: it verifies offline and broadcasts what the payer signed.
func facilitatorCmd(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 0 {
		ts.Fatalf("usage: x402facilitator")
	}

	rpcAddr := ts.Getenv("RPC_ADDR")
	if rpcAddr == "" {
		ts.Fatalf("x402facilitator: no node is running; call gnoland start first")
	}

	rpc, err := rpcclient.NewHTTPClient(httpURL(rpcAddr))
	ts.Check(err)

	node := x402gno.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc})
	server := httptest.NewServer(x402gno.NewFacilitator(node, chainID).Handler())
	ts.Defer(server.Close)

	ts.Setenv("FACILITATOR_URL", server.URL)
}

// x402seller guards one HTTP resource with the ecosystem's own middleware, with
// gno registered as a way to pay for it. It never touches the chain.
func sellerCmd(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: x402seller <payTo>")
	}
	payTo := args[0]

	facilitator := ts.Getenv("FACILITATOR_URL")
	if facilitator == "" {
		ts.Fatalf("x402seller: no facilitator is running; call x402facilitator first")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /weather", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"location": "Paris",
			"forecast": "sunny",
			"celsius":  24,
		})
	})

	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes: x402http.RoutesConfig{
			"GET /weather": {
				Accepts: x402http.PaymentOptions{{
					Scheme:  "exact",
					PayTo:   payTo,
					Price:   x402.AssetAmount{Asset: priceAsset, Amount: priceAmount},
					Network: network,
				}},
				Description: "Weather data",
				MimeType:    "application/json",
			},
		},
		Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitator}),
		Schemes: []nethttpmw.SchemeConfig{
			{Network: network, Server: gnoexact.NewExactGnoScheme()},
		},
		SyncFacilitatorOnStart: true,
	})(mux)

	server := httptest.NewServer(handler)
	ts.Defer(server.Close)

	ts.Setenv("X402_SELLER_URL", server.URL+"/weather")
	ts.Setenv("X402_GNO_RPC", ts.Getenv("RPC_ADDR"))
}

// The node reports its address in tm2's own form, which names the transport rather
// than the scheme an HTTP client needs.
func httpURL(addr string) string {
	return strings.Replace(addr, "tcp://", "http://", 1)
}
