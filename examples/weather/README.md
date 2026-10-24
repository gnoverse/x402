# weather — an HTTP resource that takes gno

The x402 seller example, with gno listed as a way to pay. The resource is weather data, made up:
each paid request gets the next of three sample readings for Paris (sunny, rain, snow), from one the
seller picks at random when it starts. It is not a realm, it makes no contract call, and it does not
know a chain exists. Everything gno-specific is a network string, one registered scheme and the page
a browser gets.

A hosted copy runs on the onyx testnet at <https://x402-demo.onyx.testnets.gno.land/weather>, at
0.25 GNOT a reading. A terminal [pays it](#paying-it) with two commands.

```go
routes := x402http.RoutesConfig{
    "GET /weather": {
        Accepts: x402http.PaymentOptions{{
            Scheme: "exact", PayTo: payTo,
            Price:   x402.AssetAmount{Asset: "ugnot", Amount: "250000"},
            Network: "gno:dev",
        }},
        Description: "Sample weather data (demo)",
    },
}

handler := nethttpmw.X402Payment(nethttpmw.Config{
    Routes:      routes,
    Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitator}),
    Schemes:     []nethttpmw.SchemeConfig{{Network: "gno:dev", Server: gnoexact.NewExactGnoScheme()}},
})(mux)
```

The middleware, the route matching, the 402 and the header encoding are all the ecosystem's. Only
`gnoexact.NewExactGnoScheme()` and the page a browser gets are ours.

## Run it

Needs Go, Node 22.18+ or 24.11+, and `gnodev` (`make install.gnodev` in a
[gnolang/gno](https://github.com/gnolang/gno) checkout). From this directory:

```sh
make demo           # a local node, the facilitator and the seller; Ctrl-C stops all three
make pay            # in a second terminal: buy.mjs pays the seller, as gno's public test1 key
```

`make demo` starts a node (chain id `dev`, RPC `:26657`), then the facilitator on `:8402`, then the
seller on `:8401`, each once the one before it answers, and keeps their logs in a temporary
directory it names. It refuses to start when something already answers at one of those addresses;
`RPC`, `FACILITATOR_LISTEN` and `LISTEN` move the node, the facilitator and the seller.
The same pieces also run one per terminal: `gnodev`, `make facilitator`, `make run`.

`make pay` prints the payment and the reading it bought:

```text
buying as g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5, gno's public test1 key; set X402_MNEMONIC to pay from your own account
paid: tx a0a66524832960c26ad3110d998f8ad87c3ee1166ca1faac145e7e1ee9ab8b8f by g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5
{"celsius":24,"forecast":"sunny","location":"Paris"}
```

### On the onyx testnet

The seller uses the public facilitator for `onyx-1`, so no local node or facilitator runs. The buyer
pays from your own key, funded at [faucet.gno.land](https://faucet.gno.land). Use a throwaway key:
the mnemonic stays in your shell history. `node buy.mjs --new-key` makes one.

```sh
make run NETWORK=gno:onyx-1 FACILITATOR=https://facilitator.onyx.testnets.gno.land PAY_TO=g1...
X402_MNEMONIC="<funded mnemonic>" make pay RPC=https://rpc.onyx.testnets.gno.land:443
```

To settle through your own facilitator instead, start it with
`make facilitator NETWORK=gno:onyx-1 RPC=https://rpc.onyx.testnets.gno.land:443` and leave
`FACILITATOR` at its default.

## Run it yourself, piece by piece

Three processes, started from the repository root. The facilitator talks to the chain; the seller
never does.

**1. A gno node**, with its RPC reachable. Any node works — a local dev chain or a testnet.

**2. The facilitator.** `-chain-id` must name the chain `-rpc` actually serves; it checks at startup
and exits rather than blame payers for an operator's mismatch.

```sh
go run ./cmd/gnofacilitator -rpc <RPC URL> -chain-id <CHAIN ID> -listen :8402
```

**3. The seller.** `-network` is `gno:` plus that same chain id.

```sh
go run ./examples/weather \
  -pay-to g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq \
  -network gno:<CHAIN ID> \
  -price 250000ugnot \
  -facilitator http://localhost:8402
```

`-price` is parsed by the chain's own coin parser, so it takes whatever `gnokey` would.

Behind a TLS proxy, set `-public-url https://<host>`: without it, every 402 names the seller's own
`http://` address. The priced route also has a rate limit all callers share, `-rate-per-second` and
`-rate-burst` (5 and 10 by default), and answers 429 past it before the payment is checked. It is a
backstop: throttle each client at the proxy, the only place that knows a client's address.
SIGINT or SIGTERM stops it after the requests in flight finish, for up to 70 s, the longest a paid
request may run; give a deployment a stop grace period above that. A clean stop exits 0 and a drain
that runs out exits 1. A second signal during the drain is ignored; SIGKILL ends it at once.

## What you get

```sh
curl -i http://localhost:8401/weather
```

`402`, with the terms in the `PAYMENT-REQUIRED` header as base64 JSON — that is where v2 puts them,
not in the body:

```json
{
  "x402Version": 2,
  "error": "Payment required",
  "resource": {
    "url": "http://localhost:8401/weather",
    "description": "Sample weather data (demo)",
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

A browser gets a page instead (`paywall.html`): a masthead naming it an unaudited, pre-release
testnet demo with sample weather data, or a local demo on a `dev` chain, and linking this
repository, then the 402, two plain sentences on what x402 and settling are, the four steps of
the exchange (402, sign, retry, 200), a dial waiting for the reading, and the terms beside it: the
price in GNOT, then in ugnot with what a ugnot is, the network and, on `onyx-1`, its faucet, the
payee, and how to pay. On a window 800px wide or less the steps and the dial follow the offer,
which stays put as they appear: the dial once paid, the steps once a payment starts. The
sentences on x402 follow it there too, so a 360x740 screen still shows the offer's first action.
Once paid, the receipt there names the reading too, since the dial is then out of view. A touch
screen pays on neither path, so there the offer says paying
takes a computer and links how it works; its main button sends the page on to one through the share
sheet or the clipboard, and a second says how to pay from code. The faucet line, Adena's install
link and the steps to it wait for that computer. The middleware's own
paywall supports EVM and Solana chains only. In a browser with the [Adena](https://adena.app)
wallet, a visitor pays from the page. A line beside the
offer says what Adena does there: it only signs, and the seller's facilitator, which holds no key,
checks the payment and broadcasts it. `paywall.js` works as a stock x402 client does: it reads the
terms, has Adena sign the bank send without broadcasting it, and repeats the request with the
payment, lighting each step as it goes. Once the payment settles, the badge reads 200, the tab's
title says the payment settled, the dial
shows the temperature and the forecast, marked as sample data, and the page takes on the forecast's
weather; the response body and the transaction show below, the transaction linked to gnoscan.io on
`onyx-1`. The offer then becomes the receipt and offers no second payment: it points on to selling
an endpoint in gno and to paying from code. A payment that may still settle, or whose answer never
came back, withdraws the button and the link to pay from code too, and keeps the steps on the
retry; a pending one links its transaction. The line beside the badge, the dial and the tab then
say the payment was sent and is not confirmed, and the status line ends on reloading to pay again
only if the payment never went through. A refusal says what happened to the payment before the
reason's code, and what to do where the page knows, then gives the button back; so does a step
Adena refused. A body that is not one of the four forecasts the page draws (sunny, cloudy, rain, snow) shows
only as served. Without Adena, the offer leads with paying from code, a link down to its steps, and
offers Adena second, with a link to adena.app and three steps: install it, create and fund an
account, then reload the page, since in Chrome Adena reaches only pages loaded after its install; without
JavaScript, only the link to paying from code shows. The steps sit behind a warning to read the
buyer first: make a throwaway key with it for the page's URL, fund the key, then run the pay command
the key step printed. The URL in those commands carries the tab's visit token, kept in the tab's
`sessionStorage`. Once a command carrying it is copied, the page asks `/weather/visits/<token>` every
3 seconds while in view, for 10 minutes after the copy or a return to the tab, and a reload asks on.
A payment it finds shows as one made with Adena does, the status line naming it paid from code by
the payer's address, shortened. Below them, the page shows how a seller lists gno on a route in Go, and where
a facilitator runs. A warning that the mechanism is unaudited and pre-release comes right before the
stock client's snippet and before the seller's.
`/` redirects to `/weather`, and `/favicon.ico` serves the page's icon, unpriced. A paid request
whose URL carries `?visit=<token>`, 32 hex characters, is kept for 15 minutes, up to 1000 at once,
the first payment under a token winning: `/weather/visits/<token>` answers with its transaction,
network, payer and the body served, or 404, never cached, on a rate limit of its own: 50 lookups a
second, bursts of 100, for all pages together, whatever `-rate-per-second` and `-rate-burst` say.

## Paying it

`buy.mjs` pays it, and one command runs it without a clone. From Go, register the
[Go mechanism](../../client/README.md#from-go) on upstream's Go client instead.

> **Read [`buy.mjs`](https://github.com/gnoverse/x402/blob/client/v0.5.0/examples/weather/buy.mjs)
> before you run it.** It signs with the mnemonic you give it, so never
> give it one that holds anything. Make a throwaway key with it, or use a
> [session key](../../client/README.md#paying-with-a-session-key), which the chain holds to its
> spend limit and expiry. Testnet only.

These commands pay the hosted seller on onyx. Any other seller's URL works in its place.

```sh
npx github:gnoverse/x402#client/v0.5.0 --new-key https://x402-demo.onyx.testnets.gno.land/weather
# fund the address it printed at https://faucet.gno.land, then run the command it printed:
X402_MNEMONIC='<mnemonic>' npx github:gnoverse/x402#client/v0.5.0 https://x402-demo.onyx.testnets.gno.land/weather
```

npx resolves the tag with git, fetches the repository at it, builds the client mechanism and runs
the root package's `x402-gno` command, which is this `buy.mjs`. It needs git and Node 22.18+ or
24.11+. The build is the package's `prepare` script, so an npm configured with `ignore-scripts` or
`strict-allow-scripts` stops before it; run the buyer from a clone instead, from the repository
root:

```sh
make js               # build the client mechanism buy.mjs imports
node examples/weather/buy.mjs --new-key https://x402-demo.onyx.testnets.gno.land/weather
X402_MNEMONIC='<mnemonic>' node examples/weather/buy.mjs https://x402-demo.onyx.testnets.gno.land/weather
```

`--new-key` prints a fresh 24-word mnemonic, the address it pays from and the command that pays
the seller URL given after it with that mnemonic, ready to paste, then exits: it sends no request
and writes no file, so the printed mnemonic is the only copy. Without a URL the command ends in a
`<seller URL>` to fill in.

To pay, it registers `ExactGnoScheme` with `@x402/core`'s stock client, admits ugnot to the
client's spend controls with a 1 GNOT cap per request, and signs with the mnemonic in
`X402_MNEMONIC`, or as gno's public test1 key when that is unset. With `X402_SESSION_MASTER` set
to an account's address, `X402_MNEMONIC` is a session key that account registered, and the payment
moves that account's coins within the session's limits.
[client/README.md](../../client/README.md#paying-with-a-session-key) shows how to register one. The
paying account must be **funded** on the chain the offer names
([faucet.gno.land](https://faucet.gno.land) funds the public testnets).

The buyer reads its account sequence from a node on the chain the offer names, so this leg needs a
real node rather than the unreachable one a 402 tolerates. It connects once it has read the offer:
to `X402_GNO_RPC` when that is set, else to the chain's public RPC when `buy.mjs` lists one
(`onyx-1`), else to a local node at `http://127.0.0.1:26657`. Set `X402_GNO_RPC` for any other
chain, to the node the facilitator uses.

The loop is covered: the e2e suite runs this seller configuration and this same `buy.mjs` against
an in-process node on every pull request, and asserts the seller's balance moved.

## What happens when you pay

`authorization` means the payment is checked, then the resource runs, then the payment is taken.
Three consequences worth knowing, all visible in `main.go`:

- **A payment the facilitator refuses gets a 402, like an unpaid request.** The buyer's 402 names
  the reason, and the middleware calls no handler for it. The seller's `OnVerifyFailure` hook logs
  it as `payment refused`, with the reason and, once the signature verified, the payer.
- **A resource that fails is never charged for.** The middleware buffers the response and skips
  settlement on any status ≥400.
- **A settlement that fails withholds the response.** The buffered body is discarded, so the cost
  of a failed settlement is the work already done rather than the data. The `ErrorHandler` is where
  a seller sees it; `SettlementHandler` is where they see the transaction that paid them.
