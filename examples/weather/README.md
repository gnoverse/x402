# weather — an HTTP resource that takes gno

The x402 seller example, with gno listed as a way to pay. The resource is weather data: it is not
a realm, it makes no contract call, and it does not know a chain exists. Everything gno-specific is
a network string and one registered scheme.

```go
routes := x402http.RoutesConfig{
    "GET /weather": {
        Accepts: x402http.PaymentOptions{{
            Scheme: "exact", PayTo: payTo,
            Price:   x402.AssetAmount{Asset: "ugnot", Amount: "250000"},
            Network: "gno:dev",
        }},
        Description: "Weather data",
    },
}

handler := nethttpmw.X402Payment(nethttpmw.Config{
    Routes:      routes,
    Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitator}),
    Schemes:     []nethttpmw.SchemeConfig{{Network: "gno:dev", Server: gnoexact.NewExactGnoScheme()}},
})(mux)
```

The middleware, the route matching, the 402 and the header encoding are all the ecosystem's. Only
`gnoexact.NewExactGnoScheme()` is ours.

## Run it

Three processes. The facilitator talks to the chain; the seller never does.

**1. A gno node**, with its RPC reachable. Any node works — a local dev chain or a testnet.

**2. The facilitator.** `-chain-id` must name the chain `-rpc` actually serves; it checks at startup
and exits rather than blame payers for an operator's mismatch.

```sh
cd go && go run ./cmd/gnofacilitator -rpc <RPC URL> -chain-id <CHAIN ID> -listen :8402
```

**3. The seller.** `-network` is `gno:` plus that same chain id.

```sh
cd go && go run ./examples/weather \
  -pay-to g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq \
  -network gno:<CHAIN ID> \
  -price 250000ugnot \
  -facilitator http://localhost:8402
```

`-price` is parsed by the chain's own coin parser, so it takes whatever `gnokey` would.

## What you get

```sh
curl -i http://localhost:8080/weather
```

`402`, with the terms in the `PAYMENT-REQUIRED` header as base64 JSON — that is where v2 puts them,
not in the body:

```json
{
  "x402Version": 2,
  "error": "Payment required",
  "resource": {
    "url": "http://localhost:8080/weather",
    "description": "Weather data",
    "mimeType": "application/json"
  },
  "accepts": [{
    "scheme": "exact",
    "network": "gno:dev",
    "asset": "ugnot",
    "amount": "250000",
    "payTo": "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
    "maxTimeoutSeconds": 300,
    "extra": { "areFeesSponsored": false }
  }]
}
```

`areFeesSponsored: false` says the payer pays the network fee inside the transaction they sign, and
the facilitator holds no key. The mechanism sets it, so no seller writes it. There is no
`paymentFlow` key because the flow is `authorization`, the specification's default, which may be
omitted.

## Paying it

There is no Go client mechanism yet, so a Go buyer cannot pay this. The JS buyer the payment test
drives can, and it is the same buyer either way:

```sh
make js                     # install and build the client mechanism
X402_SELLER_URL=http://localhost:8080/weather X402_GNO_RPC=<RPC URL> node e2e/buyer.mjs
```

It registers `ExactGnoScheme` with a stock `@x402/fetch` client — that one `register("gno:*", …)`
call is the only gno-aware line — and signs with the well-known test1 mnemonic. **Fund that account
first**, and point `X402_GNO_RPC` at the same node the facilitator uses: the buyer reads its account
sequence from the chain, so this leg needs a real node rather than the unreachable one a 402 tolerates.

The paid loop itself is covered: `e2e` runs this exact configuration — the same middleware, the
same `accepts[]` entry, the same 250000ugnot price, this same `buyer.mjs` — against an in-process
node on every pull request, and asserts the seller's balance moved. What has not been run is the
command sequence on this page: three processes started by hand against a live chain, with a funded
account.

## What happens when you pay

`authorization` means the payment is checked, then the resource runs, then the payment is taken.
Two consequences worth knowing, both visible in `main.go`:

- **A resource that fails is never charged for.** The middleware buffers the response and skips
  settlement on any status ≥400.
- **A settlement that fails withholds the response.** The buffered body is discarded, so the cost
  of a failed settlement is the work already done rather than the data. The `ErrorHandler` is where
  a seller sees it; `SettlementHandler` is where they see the transaction that paid them.
