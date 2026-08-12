// Command gnofacilitator serves the x402 facilitator API (verify/settle/
// supported) for a single gno chain. It holds no keys: it only relays
// client-signed transactions. EXPERIMENTAL.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	gnoclient "github.com/gnolang/gno/gno.land/pkg/gnoclient"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"

	"github.com/gnoverse/x402/go/facilitator"
)

// version is stamped at build time with -ldflags "-X main.version=…". A binary
// that reports "dev" was built without one, which is the honest answer: an
// operator debugging a settlement needs to know which build refused it, and
// guessing from a file date is not knowing.
var version = "dev"

// Server timeouts, chosen to bound a slow or hostile client without
// interrupting a legitimate verify/settle round trip against the chain.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
)

// How long the startup chain-id check waits for a node to answer.
//
// A facilitator is normally started alongside its node — the same compose file,
// the same unit, the same container — so the first query usually lands before the
// node is listening. Asking once would make the check permanently useless in
// exactly the deployment it ships in: it would warn every time and never compare
// anything. Retrying briefly makes it real without holding startup hostage to a
// node that is genuinely down, which is reported and served through (every request
// then answers 503, which is the honest answer anyway).
const (
	chainIDCheckWindow   = 10 * time.Second
	chainIDCheckInterval = 500 * time.Millisecond
)

// nodeChainID asks the node which chain it serves, retrying a node that is not
// answering yet. The last error is returned once the window is spent.
func nodeChainID(node *facilitator.GnoclientNode) (string, error) {
	deadline := time.Now().UTC().Add(chainIDCheckWindow)
	for attempt := 0; ; attempt++ {
		reported, err := node.NodeChainID(context.Background())
		if err == nil {
			if attempt > 0 {
				slog.Info("node answered the chain-id check", "attempts", attempt+1)
			}
			return reported, nil
		}
		if time.Now().UTC().After(deadline) {
			return "", err
		}
		time.Sleep(chainIDCheckInterval)
	}
}

func main() {
	listen := flag.String("listen", ":8402", "HTTP listen address")
	rpcURL := flag.String("rpc", "", "gno chain RPC URL (required)")
	chainID := flag.String("chain-id", "", "chain id served as network gno:<chain-id> (required)")
	ratePerSecond := flag.Float64("rate-per-second", facilitator.DefaultRatePerSecond, "sustained per-peer requests/second on /verify and /settle (0 = default)")
	rateBurst := flag.Int("rate-burst", facilitator.DefaultRateBurst, "largest per-peer request spike tolerated on /verify and /settle (0 = default)")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	// Answered before the required flags are enforced: asking a binary what it is
	// must not require knowing how to run it.
	if *showVersion {
		fmt.Println(version)
		return
	}

	if *rpcURL == "" || *chainID == "" {
		slog.Error("-rpc and -chain-id are required")
		os.Exit(2)
	}
	// The network this serves is "gno:" + the flag, so a chain id the CAIP-2 form
	// cannot carry has to be refused here: every payment would be refused instead,
	// and none of those refusals would name the flag.
	if err := facilitator.ValidChainID(*chainID); err != nil {
		slog.Error("-chain-id cannot be published as a network name", "err", err)
		os.Exit(2)
	}
	if *ratePerSecond < 0 || *rateBurst < 0 {
		slog.Error("-rate-per-second and -rate-burst must not be negative")
		os.Exit(2)
	}

	rpc, err := rpcclient.NewHTTPClient(*rpcURL)
	if err != nil {
		slog.Error("rpc client", "err", err)
		os.Exit(1)
	}
	node := facilitator.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc})

	// -chain-id and -rpc are configured separately, and a disagreement is
	// undetectable from a payment: every signature would be verified against the
	// wrong sign doc and refused as invalid, blaming payers for an operator error.
	// An answered mismatch is fatal; a query that never answered is not evidence of
	// one, so it warns rather than keeping the service from starting during a node
	// outage.
	switch reported, err := nodeChainID(node); {
	case err != nil:
		slog.Warn("cannot confirm the node's chain id; a mismatch would refuse every payment",
			"configured", *chainID, "err", err)
	case reported != *chainID:
		slog.Error("the node serves a different chain than -chain-id names",
			"configured", *chainID, "node", reported)
		os.Exit(1)
	}

	f := facilitator.New(node, *chainID,
		facilitator.WithRateLimit(facilitator.RateLimit{PerSecond: *ratePerSecond, Burst: *rateBurst}))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           f.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	slog.Info("gnofacilitator listening",
		"version", version, "addr", *listen, "network", "gno:"+*chainID)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
