// Command weather is a seller: one ordinary HTTP resource, priced, with gno
// listed as a way to pay for it.
//
// It is the x402.org front-page example with a gno accepts[] entry. The resource
// is weather data and knows nothing about the chain — no realm, no contract call.
// Everything gno-specific is two strings and one registered scheme.
//
//	weather -pay-to g1... -network gno:dev -price 250000ugnot
//
// Run a facilitator first; see the README next to this file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gnolang/gno/tm2/pkg/std"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"

	gnoexact "github.com/gnoverse/x402/mechanisms/gno/exact/server"
)

// The facilitator is queried while the middleware is being built, so this bounds
// startup rather than a request.
const facilitatorSyncTimeout = 10 * time.Second

const readHeaderTimeout = 5 * time.Second

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	facilitator := flag.String("facilitator", "http://localhost:8402", "gno facilitator base URL")
	network := flag.String("network", "gno:dev", "CAIP-2 network name, gno:<chain-id>")
	payTo := flag.String("pay-to", "", "gno address paid for each request (required)")
	price := flag.String("price", "250000ugnot", "price per request, as amount and denomination")
	flag.Parse()

	if *payTo == "" {
		fmt.Fprintln(os.Stderr, "weather: -pay-to is required; nobody is paid otherwise")
		os.Exit(2)
	}

	// gno prices are coins, so the chain's own parser decides what is payable and
	// this example never invents a second syntax for an amount.
	coin, err := std.ParseCoin(*price)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weather: -price %q is not a gno coin: %v\n", *price, err)
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /weather", serveWeather)

	routes := x402http.RoutesConfig{
		"GET /weather": {
			Accepts: x402http.PaymentOptions{{
				Scheme:  "exact",
				PayTo:   *payTo,
				Price:   x402.AssetAmount{Asset: coin.Denom, Amount: fmt.Sprint(coin.Amount)},
				Network: x402.Network(*network),
			}},
			Description: "Weather data",
			MimeType:    "application/json",
		},
	}

	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes:      routes,
		Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: *facilitator}),
		Schemes: []nethttpmw.SchemeConfig{
			{Network: x402.Network(*network), Server: gnoexact.NewExactGnoScheme()},
		},
		SyncFacilitatorOnStart: true,
		Timeout:                facilitatorSyncTimeout,

		// Settlement runs after the resource has produced its response, so this is
		// where a seller learns they were actually paid.
		SettlementHandler: func(_ http.ResponseWriter, _ *http.Request, resp *x402.SettleResponse) {
			slog.Info("paid", "tx", resp.Transaction, "payer", resp.Payer, "network", resp.Network)
		},

		// And this is where they learn they were not. The buyer is not served: the
		// middleware buffers the resource and discards it when settlement fails, so
		// the cost of a failure here is the work already done, not the data.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Error("not paid; the response is withheld", "err", err)
			w.WriteHeader(http.StatusPaymentRequired)
		},
	})(mux)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	slog.Info("weather seller listening",
		"addr", *listen, "network", *network, "price", coin.String(), "payTo", *payTo)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func serveWeather(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"location": "Paris",
		"forecast": "sunny",
		"celsius":  24,
	})
}
