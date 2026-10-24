// Command weather is a seller: one ordinary HTTP resource, priced, with gno
// listed as a way to pay for it.
//
// It is the x402.org front-page example with a gno accepts[] entry. The resource
// is weather data and knows nothing about the chain — no realm, no contract call.
// Everything gno-specific is two strings, one registered scheme, and the page a
// browser gets in place of the resource.
//
//	weather -pay-to g1... -network gno:dev -price 250000ugnot
//
// Run a facilitator first; see the README next to this file.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gnolang/gno/tm2/pkg/std"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"

	gnoexact "github.com/gnoverse/x402/server/exact"
)

// paymentTimeout bounds a paid request as a whole: verify, the resource, and a
// settle that can wait up to 10 s for a block, then run once more when the
// facilitator answers settlement_pending. It also bounds the facilitator query
// made while the middleware is built.
const paymentTimeout = 60 * time.Second

// Server timeouts. The write timeout runs from the end of the request's headers
// to the end of the response, so it leaves a paid request its paymentTimeout
// and room to write the answer.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = paymentTimeout + 10*time.Second
	idleTimeout       = 60 * time.Second
)

// paymentScheme is the x402 scheme the weather is priced in.
const paymentScheme = "exact"

// weatherPath is where the resource is served, and the path a public URL is
// extended with.
const weatherPath = "/weather"

// paywallPage is what a browser gets in place of the resource. The
// middleware's own paywall supports EVM and Solana chains only.
//
//go:embed paywall.html
var paywallPage string

var paywall = template.Must(template.New("paywall").Parse(paywallPage))

// paywallScript is the page's payment logic, inlined so the page makes no
// request beyond the seller.
//
//go:embed paywall.js
var paywallScript string

// favicon is the page's icon, which a browser asks for at /favicon.ico on its
// own.
//
//go:embed favicon.ico
var favicon []byte

func main() {
	listen := flag.String("listen", ":8401", "HTTP listen address")
	facilitator := flag.String("facilitator", "http://localhost:8402", "gno facilitator base URL")
	network := flag.String("network", "gno:dev", "CAIP-2 network name, gno:<chain-id>")
	payTo := flag.String("pay-to", "", "gno address paid for each request (required)")
	price := flag.String("price", "250000ugnot", "price per request, as amount and denomination")
	publicURL := flag.String("public-url", "", "base URL buyers reach this seller at, such as https://weather.example behind a TLS proxy (default: the address each request arrived on)")
	ratePerSecond := flag.Float64("rate-per-second", defaultRatePerSecond, "sustained requests/second on the priced route, all callers together (0 = default)")
	rateBurst := flag.Int("rate-burst", defaultRateBurst, "largest request spike tolerated on the priced route, all callers together (0 = default)")
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

	seller, err := newSeller(context.Background(), sellerConfig{
		facilitatorURL: *facilitator,
		network:        *network,
		payTo:          *payTo,
		price:          coin,
		log:            slog.Default(),
		publicURL:      *publicURL,
		ratePerSecond:  *ratePerSecond,
		rateBurst:      *rateBurst,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "weather: %v\n", err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("weather seller listening",
		"addr", *listen, "network", *network, "price", coin.String(), "payTo", *payTo)
	if err := serve(ctx, newServer(*listen, seller), ln, drainTimeout); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
	slog.Info("weather seller stopped")
}

type sellerConfig struct {
	facilitatorURL string
	network        string // gno:<chain-id>
	payTo          string
	price          std.Coin
	log            *slog.Logger
	publicURL      string  // base URL buyers reach the seller at; "" names each request's own address
	ratePerSecond  float64 // backstop on the priced route; <=0 takes defaultRatePerSecond
	rateBurst      int     // <=0 takes defaultRateBurst
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// newSeller returns the weather resource behind the x402 middleware, behind a
// rate limit. It asks the facilitator what it supports before returning: a
// facilitator it cannot reach then is warned about and asked again in the
// background, until it serves exact on cfg.network or ctx ends. Only one
// reachable then without exact on cfg.network exits the process.
func newSeller(ctx context.Context, cfg sellerConfig) (http.Handler, error) {
	route := "GET " + weatherPath
	mux := http.NewServeMux()
	readings := sampleReadings()
	mux.HandleFunc(route, rotatingWeather(readings, rand.IntN(len(readings))))

	// Behind a TLS proxy the middleware would name an http:// URL on the
	// seller's own address, so the 402 names the public one instead.
	var resource string
	if cfg.publicURL != "" {
		base, err := url.Parse(cfg.publicURL)
		if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
			base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
			return nil, fmt.Errorf("-public-url %q is not a plain absolute http or https URL", cfg.publicURL)
		}
		resource = base.JoinPath(weatherPath).String()
	}

	amount, unit := shownAmount(cfg.price)
	chainID := strings.TrimPrefix(cfg.network, "gno:")
	var page strings.Builder
	err := paywall.Execute(&page, struct {
		Amount, Unit, Coin, Network, ChainID, PayTo, TxURL, Faucet, Demo string
		Script                                                           template.JS
	}{
		Amount:  amount,
		Unit:    unit,
		Coin:    cfg.price.String(),
		Network: cfg.network,
		ChainID: chainID,
		PayTo:   cfg.payTo,
		TxURL:   explorerTxURL(chainID),
		Faucet:  faucetHost(chainID),
		Demo:    demoLabel(chainID),
		Script:  template.JS(paywallScript),
	})
	if err != nil {
		return nil, fmt.Errorf("render the paywall: %w", err)
	}

	routes := x402http.RoutesConfig{
		route: {
			Resource: resource,
			Accepts: x402http.PaymentOptions{{
				Scheme:  paymentScheme,
				PayTo:   cfg.payTo,
				Price:   x402.AssetAmount{Asset: cfg.price.Denom, Amount: fmt.Sprint(cfg.price.Amount)},
				Network: x402.Network(cfg.network),
			}},
			Description:       "Sample weather data (demo)",
			MimeType:          "application/json",
			CustomPaywallHTML: page.String(),
		},
	}

	facilitator := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: cfg.facilitatorURL})
	server := x402.Newx402ResourceServer(
		x402.WithFacilitatorClient(facilitator),

		// The middleware answers a payment the facilitator refuses with a 402 and
		// calls no handler for it, so this hook is the only place a seller learns of
		// it. A payment that never reached the facilitator does not get here.
		x402.WithOnVerifyFailureHook(func(failure x402.VerifyFailureContext) (*x402.VerifyFailureHookResult, error) {
			if refusal, ok := errors.AsType[*x402.VerifyError](failure.Error); ok {
				cfg.log.Warn("payment refused",
					"reason", refusal.InvalidReason, "payer", refusal.Payer, "detail", refusal.InvalidMessage)
			} else {
				cfg.log.Error("payment not checked", "err", failure.Error)
			}
			return nil, nil
		}),
	).Register(x402.Network(cfg.network), gnoexact.NewExactGnoScheme())

	paid := nethttpmw.PaymentMiddleware(routes, server,
		nethttpmw.WithSyncFacilitatorOnStart(true),
		nethttpmw.WithTimeout(paymentTimeout),

		// Settlement runs after the resource has produced its response, so this is
		// where a seller learns they were actually paid.
		nethttpmw.WithSettlementHandler(func(_ http.ResponseWriter, _ *http.Request, resp *x402.SettleResponse) {
			cfg.log.Info("paid", "tx", resp.Transaction, "payer", resp.Payer, "network", resp.Network)
		}),

		// And this is where they learn a settlement failed. The buyer is not served:
		// the middleware buffers the resource and discards it when settlement fails,
		// so the cost of a failure here is the work already done, not the data.
		nethttpmw.WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, err error) {
			cfg.log.Error("not paid; the response is withheld", "err", err)
			w.WriteHeader(http.StatusPaymentRequired)
		}),
	)(mux)
	keepSyncing(ctx, server, facilitator, cfg)

	// The limit runs before the middleware, so a refused request costs the
	// facilitator nothing.
	visits := newVisitBook()
	limited := http.NewServeMux()
	limited.Handle(route, newRateLimit(cfg.ratePerSecond, cfg.rateBurst).guard(visits.recordSettled(paid)))
	limited.Handle("GET "+weatherPath+"/visits/{token}",
		newRateLimit(lookupRatePerSecond, lookupRateBurst).guard(visits.lookupHandler()))

	// Someone who types the host alone gets the demo: at the public URL when the
	// seller knows it, since a proxy that strips a prefix with no trailing slash
	// would send a relative Location outside the prefix, and relative otherwise.
	location := strings.TrimPrefix(weatherPath, "/")
	if resource != "" {
		location = resource
	}
	limited.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusSeeOther)
	})
	limited.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(favicon)
	})
	return limited, nil
}

// ugnotPerGNOT is how many ugnot make a GNOT; the %06d below writes the
// fraction in as many digits.
const ugnotPerGNOT = 1_000_000

// shownAmount is how the page writes a price: ugnot in GNOT, the unit people
// read, and any other denomination as it is.
func shownAmount(coin std.Coin) (amount, unit string) {
	if coin.Denom != "ugnot" {
		return strconv.FormatInt(coin.Amount, 10), coin.Denom
	}
	whole, fraction := coin.Amount/ugnotPerGNOT, coin.Amount%ugnotPerGNOT
	if fraction == 0 {
		return strconv.FormatInt(whole, 10), "GNOT"
	}
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%06d", fraction), "0")), "GNOT"
}

// reading is the weather resource's body.
type reading struct {
	Celsius  int    `json:"celsius"`
	Forecast string `json:"forecast"`
	Location string `json:"location"`
}

// sampleReadings are the made-up readings the seller serves: it is a demo and
// reads no weather.
func sampleReadings() []reading {
	return []reading{
		{Celsius: 24, Forecast: "sunny", Location: "Paris"},
		{Celsius: 14, Forecast: "rain", Location: "Paris"},
		{Celsius: 1, Forecast: "snow", Location: "Paris"},
	}
}

// rotatingWeather serves readings in turn from readings[start], one per request
// it is handed. Behind the middleware that is one per paid request, a refused
// settlement included.
func rotatingWeather(readings []reading, start int) http.HandlerFunc {
	var served atomic.Uint64
	return func(w http.ResponseWriter, _ *http.Request) {
		r := readings[(uint64(start)+served.Add(1)-1)%uint64(len(readings))]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r)
	}
}

// explorerTxURL returns the page that shows one of chainID's transactions once
// its hash is appended, or "" for a chain with no public explorer.
func explorerTxURL(chainID string) string {
	switch chainID {
	case "onyx-1":
		return "https://gnoscan.io/transactions/details?chainId=onyx-1&txhash="
	}
	return ""
}

// faucetHost returns the host of the faucet that hands out chainID's test GNOT,
// or "" for a chain no public faucet serves.
func faucetHost(chainID string) string {
	switch chainID {
	case "onyx-1":
		return "faucet.gno.land"
	}
	return ""
}

// demoLabel names the demo by where chainID runs: dev is the chain gnodev and
// gnoland start by default, local to whoever runs them, and any other a testnet.
func demoLabel(chainID string) string {
	if chainID == "dev" {
		return "local demo"
	}
	return "testnet demo"
}
