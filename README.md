# x402 on gno.land

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Container](https://img.shields.io/badge/ghcr.io-gnoverse%2Fgnofacilitator-2496ED?logo=docker&logoColor=white)](https://github.com/gnoverse/x402/pkgs/container/gnofacilitator)

> Charge for an HTTP endpoint in ugnot, and let any x402 client pay it.

[x402](https://x402.org) is the payment protocol behind HTTP 402: a server answers `402` with the
terms it accepts, a client pays, and the request succeeds. This makes **gno.land one of those
terms**. The resource is any endpoint — the chain is only the rail, so nothing here needs a realm or
a contract call.

It is a *mechanism inside* x402, not a second implementation of it. Route matching, the 402, header
encoding and the retry are the ecosystem's own middleware; this supplies the two seams x402 reserves
for a chain.

- **Seller mechanism** — list gno in a route's `accepts[]` and the canonical middleware prices it, offers it and reads the payment.
- **`gnofacilitator`** — verifies payments and broadcasts them. It holds no keys.
- **Client mechanism** (`js/`) — one `register()` call teaches a stock x402 client to pay any gno chain.

> [!WARNING]
> **Work in progress — unaudited and pre-release.**
>
> - The gno scheme is **not upstream yet**; no `specs/schemes/exact/scheme_exact_gno.md` is merged.
> - Point it at testnets. Nothing here has been reviewed for real funds.
> - The client package is not published to a registry yet — build it from `js/`.
>
> File issues when something looks off.

## Sell something

A seller writes a route and lists gno as a way to pay for it. This is the x402 seller snippet, with
two gno-specific strings and one registered scheme:

```go
routes := x402http.RoutesConfig{
    "GET /weather": {
        Accepts: x402http.PaymentOptions{{
            Scheme:  "exact",
            PayTo:   "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
            Price:   x402.AssetAmount{Asset: "ugnot", Amount: "250000"},
            Network: "gno:dev",
        }},
        Description: "Weather data",
    },
}

handler := nethttpmw.X402Payment(nethttpmw.Config{
    Routes:      routes,
    Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitatorURL}),
    Schemes:     []nethttpmw.SchemeConfig{{Network: "gno:dev", Server: gnoexact.NewExactGnoScheme()}},
})(mux)
```

An unpaid request gets `402` with the terms in the `PAYMENT-REQUIRED` header — base64 JSON, which is
where v2 puts them, not in the body:

```json
{ "x402Version": 2,
  "resource": { "url": "http://localhost:8080/weather", "description": "Weather data" },
  "accepts": [{
    "scheme": "exact", "network": "gno:dev", "asset": "ugnot", "amount": "250000",
    "payTo": "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
    "maxTimeoutSeconds": 300,
    "extra": { "areFeesSponsored": false }
  }] }
```

Runnable, with a facilitator and a real chain → **[examples/weather](examples/weather)**.

## Pay for it

A buyer's only gno-aware line is the registration. Everything else — recognising the 402, selecting
an entry, encoding `PAYMENT-SIGNATURE`, retrying — is `@x402/fetch`'s own code, unmodified:

```js
const client = new x402Client().register("gno:*", new ExactGnoScheme(wallet));
const paid = wrapFetchWithPayment(fetch, client);
const res = await paid("https://api.example.com/weather");
```

`"gno:*"` means it. The payment is signed against the chain id the **offer** names, so one client
pays `gno:test14` and `gno:dev` without being reconfigured — see [js/README.md](js/README.md).

## What's in here

| Path | What |
|------|------|
| `mechanisms/gno/exact/server/` | the seller mechanism — upstream's `SchemeNetworkServer` for gno |
| `cmd/gnofacilitator/` | the keyless facilitator: `/verify`, `/settle`, `/supported` |
| `js/` | `@gnoverse/x402-gno` — the client mechanism, TypeScript |
| `examples/weather/` | a priced HTTP endpoint you can run and curl |
| `e2e/` | one real payment, through a real node — its own Go module |
| root package | the wire types, verification, and settlement this repo started as |

## The fee is yours

- **The payer pays the network fee** — `std.Fee` names no payer and the chain charges the first signer, so the buyer of the resource is also the buyer of the gas. Every gno offer says so: `extra.areFeesSponsored` is `false`.
- **The facilitator holds no key** — it verifies offline and broadcasts a transaction the payer already signed. A compromised facilitator cannot move funds anywhere the payer did not sign for.
- **A failed resource is never charged for** — the flow is `authorization`, the specification's default: verify, serve, then settle. The middleware buffers the response and skips settlement on any status ≥400.
- **A failed settlement withholds the response** — the buffered body is discarded, so the cost of a settlement failure is the work already done rather than the data.
- **Prices name their denomination** — gno has no default asset, so `"$0.001"` is refused rather than resolved into a token nobody agreed on.

**The risk this accepts:** between `/verify` and `/settle` a payer can consume the signed-over
account sequence with another transaction. gno has no pull-settlement primitive, so this is
cooperative — the same exposure XRPL ships and documents.

## Development

```bash
make test                # Library tests (no chain, no npm)
make js-test             # Client mechanism, including the sign-doc byte-equality check
make test-e2e            # One real payment against an in-process node
make lint                # go vet (both modules) + gofmt -l
make build               # bin/gnofacilitator
make help                # Every target
```

`make test-e2e` is the one that matters: it starts a real node via gno.land's txtar harness, stands
up the facilitator and a priced endpoint, pays with the JS client, and asserts the seller's balance
changed. Every other test proves one part.

Built against [`github.com/x402-foundation/x402/go/v2`](https://github.com/x402-foundation/x402) for
the protocol and [`gnolang/gno`](https://github.com/gnolang/gno) for the chain.

## Conformance

Standard: the `exact` scheme, the `authorization` flow, v2 headers, `gno:<chain-id>` as a CAIP-2
network, prices in native units — the convention chains without a default stablecoin already use.

Deliberately not, for now: dollar-string pricing, a browser paywall, payload `extensions`, and a
CAIP-2 namespace registration (verified not required — `near:testnet` ships without one).

## License

Apache-2.0. See [LICENSE](LICENSE).
