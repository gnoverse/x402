# x402 on gno.land

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Container](https://img.shields.io/badge/ghcr.io-gnoverse%2Fgnofacilitator-2496ED?logo=docker&logoColor=white)](https://github.com/gnoverse/x402/pkgs/container/gnofacilitator)

> Charge for an HTTP endpoint in ugnot, and let any x402 client pay it.

[x402](https://x402.org) is the payment protocol behind HTTP 402: a server answers `402` with the
terms it accepts, a client pays, and the request succeeds. This adds gno.land as one of those terms.
The resource is any endpoint — no realm and no contract call are involved.

The protocol, route matching, header encoding and the retry come from the ecosystem's own
middleware. This supplies the chain-specific parts:

- **Seller mechanism** — list gno in a route's `accepts[]` and the middleware prices it, offers it and reads the payment.
- **`gnofacilitator`** — verifies payments and broadcasts them. It holds no keys.
- **Client mechanism** — one `register()` call lets a stock x402 client pay a gno chain.

> [!WARNING]
> **Work in progress — unaudited and pre-release.**
>
> - No gno scheme document is merged upstream yet.
> - Point it at testnets. Nothing here has been reviewed for real funds.
> - The client package is not published to a registry — build it with `make js` from the repository root.

## Sell something

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

An unpaid request gets `402`, with the terms in the `PAYMENT-REQUIRED` header as base64 JSON — v2
carries them there, not in the body:

```json
{ "x402Version": 2,
  "error": "Payment required",
  "resource": { "url": "http://localhost:8080/weather", "description": "Weather data",
                "mimeType": "application/json" },
  "accepts": [{
    "scheme": "exact", "network": "gno:dev", "asset": "ugnot", "amount": "250000",
    "payTo": "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
    "maxTimeoutSeconds": 300,
    "extra": { "areFeesSponsored": false }
  }] }
```

Runnable, against a facilitator and a real chain → **[go/examples/weather](go/examples/weather)**.

## Pay for it

```js
import { ExactGnoScheme } from "@gnoverse/x402-gno/exact/client";

const client = new x402Client().register("gno:*", new ExactGnoScheme(wallet));
const paid = wrapFetchWithPayment(fetch, client);
const res = await paid("https://api.example.com/weather");
```

Recognising the 402, selecting an entry, encoding `PAYMENT-SIGNATURE` and retrying are
`@x402/fetch`'s own code, unmodified. The payment is signed against the chain id the offer names, so
one client pays `gno:test14` and `gno:dev` without reconfiguration — see [js/README.md](js/README.md).

## What's in here

An x402 mechanism has three roles — **server** (the seller), **facilitator**, **client** (the buyer)
— and the ecosystem names them the same way in both languages: `@x402/evm` publishes
`./exact/server`, `./exact/facilitator` and `./exact/client`, and its Go module has the matching
directories. This fills the same grid for gno; which language a role is written in is an
implementation detail of that role.

| Role | Path | What |
|------|------|------|
| server | `go/mechanisms/gno/exact/server/` | upstream's `SchemeNetworkServer`, implemented for gno |
| facilitator | `go/facilitator/` | verification, settlement, and the `/verify` `/settle` `/supported` service |
| client | `js/src/exact/client.ts` | `@gnoverse/x402-gno/exact/client`, TypeScript |

Plus `go/cmd/gnofacilitator/` (the binary), `go/examples/weather/` (a priced endpoint you can run
and curl) and `go/e2e/` (one real payment through a real node — its own Go module).

Under that, sources split by language into `go/` and `js/` the way
[x402-foundation/x402](https://github.com/x402-foundation/x402) splits its own. Both manifests —
`go.mod` and `package.json` — live at the root, so `go test ./...`, `npm test` and every `make`
target run from here.

## Payment model

- **The payer pays the network fee.** `std.Fee` names no payer and the chain charges the first signer, so the payer of the resource is also the payer of the gas. Offers state this as `extra.areFeesSponsored: false`.
- **The facilitator holds no key.** It verifies offline and broadcasts a transaction the payer already signed, so it cannot move funds anywhere the payer did not sign for.
- **A failed resource is not charged for.** The flow is `authorization`: verify, serve, then settle. The middleware buffers the response and skips settlement on any status ≥400.
- **A failed settlement withholds the response.** The buffered body is discarded.
- **Prices name their denomination.** gno has no default asset, so `"$0.001"` errors rather than resolving to a token.

**Accepted risk:** between `/verify` and `/settle` a payer can consume the signed-over account
sequence with another transaction. gno has no pull-settlement primitive, so this is cooperative — the
same exposure XRPL documents for its own `exact` mechanism.

## Development

```bash
make test                # Library tests (no chain, no npm)
make js-test             # Client mechanism, including the sign-doc byte-equality check
make test-e2e            # One real payment against an in-memory node
make lint                # go vet (both modules) + gofmt -l
make build               # bin/gnofacilitator
make help                # Every target
```

`make test-e2e` starts a node via gno.land's txtar harness, stands up the facilitator and a priced
endpoint, pays with the JS client, and asserts the seller's balance changed.

Built against [`github.com/x402-foundation/x402/go/v2`](https://github.com/x402-foundation/x402) for
the protocol and [`gnolang/gno`](https://github.com/gnolang/gno) for the chain.

## Conformance

Implemented: the `exact` scheme, the `authorization` flow, v2 headers, `gno:<chain-id>` as a CAIP-2
network, prices in native units.

Not implemented: dollar-string pricing, a browser paywall, payload `extensions`, and a CAIP-2
namespace registration — the last is not required by the schema (`near:testnet` ships without one).

## License

Apache-2.0. See [LICENSE](LICENSE).
