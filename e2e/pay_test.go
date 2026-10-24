// Package e2e joins the halves. Every other test in this repo proves one part —
// the seller emits a conformant 402, the ordering is authorization, a JS-signed
// payment satisfies the static rules, the buyer's sign doc matches the wallet's.
// None of them can say a payment happened, because that needs a chain.
//
// This drives gno.land's txtar harness, which starts a real in-memory node, and
// adds two commands to it: one that stands up the facilitator against that node,
// one that guards an HTTP resource with the canonical middleware. The buyer here is
// the JS one, run by `exec node`, so the assertion crosses both languages;
// goclient_test.go drives the same commands with the Go one.
//
// settle_test.go starts in-process nodes itself instead, to settle one signed
// payment twice at once and to replay a settled one, which a scripted buyer
// paying once cannot do.
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/gno.land/pkg/integration"
	"github.com/gnolang/gno/gnovm/pkg/gnoenv"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"

	"github.com/gnoverse/x402/facilitator"
	"github.com/gnoverse/x402/facilitator/exact"
	gnoexact "github.com/gnoverse/x402/server/exact"
)

// The harness's in-memory node serves this chain, so it is the CAIP-2 reference
// every offer and every signature in the script is bound to.
const (
	chainID = "tendermint_test"
	network = "gno:" + chainID
)

func TestAStockClientPaysAGnoSeller(t *testing.T) {
	buyer := buyerScript(t)

	exampleBuyer := exampleBuyerScript(t)

	params := testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			env.Setenv("X402_BUYER", buyer)
			env.Setenv("X402_EXAMPLE_BUYER", exampleBuyer)
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"x402facilitator": facilitatorCmd,
			"x402seller":      sellerCmd,
		},
	}

	writableGnoRoot(t)
	// Wraps our Setup and merges our Cmds rather than replacing either.
	require.NoError(t, integration.SetupGnolandTestscript(t, &params))
	testscript.Run(t, params)
}

// writableGnoRoot points GNOROOT at a mirror of the gno module made of symlinks.
// A node writes its peer address book under <GNOROOT>/config, and the module
// cache GNOROOT otherwise resolves to is read-only.
func writableGnoRoot(t *testing.T) {
	t.Helper()
	mirror, err := gnoRootMirror()
	require.NoError(t, err)
	t.Setenv("GNOROOT", mirror)
}

// gnoRootMirror makes the mirror once per process. gno loads the stdlib once per
// process and panics when a later load names another directory, so a second
// mirror, made for a second run of the same test, panics on load.
var gnoRootMirror = sync.OnceValues(func() (string, error) {
	root, err := gnoenv.GuessRootDir()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	mirror, err := os.MkdirTemp("", "x402-e2e-gnoroot-")
	if err != nil {
		return "", err
	}
	madeMirror = mirror
	for _, e := range entries {
		if err := os.Symlink(filepath.Join(root, e.Name()), filepath.Join(mirror, e.Name())); err != nil {
			return "", err
		}
	}
	return mirror, nil
})

// madeMirror is the mirror gnoRootMirror made, if any, for TestMain to remove.
var madeMirror string

func TestMain(m *testing.M) {
	code := m.Run()
	if madeMirror != "" {
		_ = os.RemoveAll(madeMirror)
	}
	os.Exit(code)
}

// buyerScript is the harness buyer. It reports what happened as JSON, which the
// contract scenarios grep for status, payer, transaction and reason.
//
// A missing buyer.mjs FAILS rather than skips, because it is committed. Only the
// built mechanism is allowed to be absent, since building it needs npm and a
// Go-only checkout should not be blocked on that.
func buyerScript(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	buyer := filepath.Join(wd, "buyer.mjs")
	_, err = os.Stat(buyer)
	require.NoError(t, err, "the JS buyer is committed, so this path is wrong")

	// buyer.mjs imports the mechanism by package name, so the package has to be
	// installed and built, not merely present in the tree. The emit lands in
	// client/dist, which the root package.json's exports map points at.
	if _, err := os.Stat(filepath.Join(wd, "..", "client", "dist", "client.mjs")); err != nil {
		t.Skipf("the JS mechanism is not built (%v); run `make js`", err)
	}
	return buyer
}

// exampleBuyerScript is the buyer shipped with the weather example. The happy-path,
// session, pending-settlement, refused-settlement, duplicate-settlement and
// unfunded-payer scenarios drive it instead of the harness buyer, so the file a
// reader lands on first is the one CI proves — the example depends on nothing, and
// this test is what may depend on the example.
func exampleBuyerScript(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	buyer := filepath.Join(wd, "..", "examples", "weather", "buy.mjs")
	_, err = os.Stat(buyer)
	require.NoError(t, err, "the example buyer is committed, so this path is wrong")
	return buyer
}

// x402facilitator stands up the facilitator against the running node. It holds no
// key: it verifies offline and broadcasts what the payer signed. With pending, the
// node it reads never reports a block executing a payment, so every settlement is
// broadcast and answered settlement_pending.
func facilitatorCmd(ts *testscript.TestScript, neg bool, args []string) {
	pending := len(args) == 1 && args[0] == "pending"
	if neg || (len(args) != 0 && !pending) {
		ts.Fatalf("usage: x402facilitator [pending]")
	}

	rpcAddr := ts.Getenv("RPC_ADDR")
	if rpcAddr == "" {
		ts.Fatalf("x402facilitator: no node is running; call gnoland start first")
	}

	rpc, err := rpcclient.NewHTTPClient(httpURL(rpcAddr))
	ts.Check(err)

	var node exact.Node = exact.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc})
	if pending {
		node = blindNode{node}
	}
	server := httptest.NewServer(facilitator.New(node, chainID).Handler())
	ts.Defer(server.Close)

	ts.Setenv("FACILITATOR_URL", server.URL)
}

// blindNode never sees a block execute the payment it is asked about.
type blindNode struct{ exact.Node }

func (blindNode) AwaitDelivery(context.Context, string) error {
	return errors.New("no block seen executing the payment")
}

// sellerOffer is a scenario's whole seller: the route, what it costs, and what a
// paying buyer receives. It is read from a file inside the script's own archive,
// so a scenario is one readable file rather than a script whose subject is
// hardcoded in Go.
//
// The network is deliberately absent. It is the chain the harness started, not
// something a scenario chooses, and a value here could only disagree with it.
type sellerOffer struct {
	Route string `json:"route"` // a net/http mux pattern, "GET /weather"
	Price struct {
		Asset  string `json:"asset"`
		Amount string `json:"amount"`
	} `json:"price"`
	Description string          `json:"description"`
	MimeType    string          `json:"mimeType"`
	Body        json.RawMessage `json:"body"` // served verbatim to a paying buyer

	// Memo, when set, is published as extra.memo and binds the payment's
	// transaction memo to it. A buyer that ignores it produces a payment the
	// facilitator refuses, so a settlement is itself proof the buyer honored it.
	Memo string `json:"memo"`

	// Status is the status the resource answers with; zero means 200. A status at
	// or above 400 makes the middleware cancel settlement instead of taking the
	// payment, which is the guarantee a paid endpoint fronting something fallible
	// depends on.
	Status int `json:"status"`

	// RefusedSettlement, when set, is the receipt a paid request is answered
	// with in place of being verified and settled: a facilitator's refusal
	// relayed with its transaction hash, as @x402/core's TypeScript server
	// relays one. Upstream's Go middleware keeps the hash only for
	// settlement_pending, so a scenario cannot get this from it.
	RefusedSettlement *refusedSettlement `json:"refusedSettlement"`
}

// refusedSettlement is what a scenario decides of a failed settle receipt; the
// seller marks it unsuccessful and names the harness's network.
type refusedSettlement struct {
	ErrorReason  string `json:"errorReason"`
	ErrorMessage string `json:"errorMessage"`
	Payer        string `json:"payer"`
	Transaction  string `json:"transaction"`
}

// readSellerOffer decodes the offer file the script names. Unknown fields are
// refused: the file is the scenario's contract, so a stray key has to fail loudly
// rather than leave a default in place and quietly change what is tested. A key
// differing only in case still binds, because encoding/json matches tags
// case-insensitively, which is why the required fields are checked by value below
// rather than left to the decoder.
func readSellerOffer(ts *testscript.TestScript, path string) sellerOffer {
	decoder := json.NewDecoder(strings.NewReader(ts.ReadFile(path)))
	decoder.DisallowUnknownFields()

	var offer sellerOffer
	if err := decoder.Decode(&offer); err != nil {
		ts.Fatalf("x402seller: %s: %v", path, err)
	}
	method, target, found := strings.Cut(offer.Route, " ")
	switch {
	case !found || method == "" || !strings.HasPrefix(target, "/"):
		ts.Fatalf("x402seller: %s: route %q is not \"<METHOD> /<path>\"", path, offer.Route)
	case offer.Price.Asset == "" || offer.Price.Amount == "":
		ts.Fatalf("x402seller: %s: the offer names no price", path)
	case len(offer.Body) == 0:
		ts.Fatalf("x402seller: %s: the offer names no body, so a paid request buys nothing", path)
	}
	return offer
}

// x402seller guards one HTTP resource with the ecosystem's own middleware, with
// gno registered as a way to pay for it. It never touches the chain.
//
// Only the middleware wiring is here. Everything the scenario decides comes from
// the offer file, because that wiring is what this test exercises and the resource
// behind it is a fixture.
func sellerCmd(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: x402seller <offer.json> <payTo>")
	}
	offer, payTo := readSellerOffer(ts, args[0]), args[1]

	facilitator := ts.Getenv("FACILITATOR_URL")
	if facilitator == "" {
		ts.Fatalf("x402seller: no facilitator is running; call x402facilitator first")
	}

	// The same route string patterns the mux and keys the priced routes, so the
	// resource served and the resource charged for cannot drift apart.
	mux := http.NewServeMux()
	mux.HandleFunc(offer.Route, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", offer.MimeType)
		if offer.Status != 0 {
			w.WriteHeader(offer.Status)
		}
		_, _ = w.Write(offer.Body)
	})

	// extra.memo is omitted entirely rather than sent empty: an absent memo binds
	// nothing, and publishing "" would bind every payment to the empty memo.
	var extra map[string]interface{}
	if offer.Memo != "" {
		extra = map[string]interface{}{"memo": offer.Memo}
	}

	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes: x402http.RoutesConfig{
			offer.Route: {
				Accepts: x402http.PaymentOptions{{
					Scheme:  "exact",
					PayTo:   payTo,
					Price:   x402.AssetAmount{Asset: offer.Price.Asset, Amount: offer.Price.Amount},
					Network: network,
					Extra:   extra,
				}},
				Description: offer.Description,
				MimeType:    offer.MimeType,
			},
		},
		Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitator}),
		Schemes: []nethttpmw.SchemeConfig{
			{Network: network, Server: gnoexact.NewExactGnoScheme()},
		},
		SyncFacilitatorOnStart: true,
	})(mux)
	if refusal := offer.RefusedSettlement; refusal != nil {
		receipt, err := json.Marshal(x402.SettleResponse{
			Success:      false,
			ErrorReason:  refusal.ErrorReason,
			ErrorMessage: refusal.ErrorMessage,
			Payer:        refusal.Payer,
			Transaction:  refusal.Transaction,
			Network:      network,
		})
		ts.Check(err)
		handler = refusingSettlement(handler, base64.StdEncoding.EncodeToString(receipt))
	}

	server := httptest.NewServer(handler)
	ts.Defer(server.Close)

	// The buyer is told both halves of the route, so a scenario changes the method
	// by editing its offer file rather than the script or this command.
	method, target, _ := strings.Cut(offer.Route, " ")
	ts.Setenv("X402_SELLER_URL", server.URL+target)
	ts.Setenv("X402_METHOD", method)
	ts.Setenv("X402_GNO_RPC", ts.Getenv("RPC_ADDR"))
}

// refusingSettlement answers a paid request the way @x402/core's TypeScript
// server answers one its facilitator refused to settle: 402 with an empty JSON
// body, and receipt as its PAYMENT-RESPONSE. An unpaid request goes to next, so
// the buyer signs against the middleware's own 402.
func refusingSettlement(next http.Handler, receipt string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PAYMENT-SIGNATURE") == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("PAYMENT-RESPONSE", receipt)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte("{}"))
	})
}

// The node reports its address in tm2's own form, which names the transport rather
// than the scheme an HTTP client needs.
func httpURL(addr string) string {
	return strings.Replace(addr, "tcp://", "http://", 1)
}
