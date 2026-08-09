# x402 on gno.land — topaz-1 demo runbook

> **EXPERIMENTAL.** Unaudited, GNOT-only, and rides the session `allow_send` path, which is
> itself young (see [docs/adr/session_authorization.md](../docs/adr/session_authorization.md)
> and [docs/adr/x402_scheme.md](../docs/adr/x402_scheme.md)). Do not point this at funds you
> can't afford to lose.

This walks the whole payment loop against the public `topaz-1` testnet: a facilitator that
relays signed payments, a seller endpoint that gates content behind one, and gnomcp signing
the payment from a user-authorized session.

## 1. Build the binaries

```bash
go build ./cmd/gnofacilitator ./cmd/gnowars
# or, for ad-hoc runs: go run ./cmd/gnofacilitator ...   /   go run ./cmd/gnowars ...
```

## 2. Run the facilitator against topaz-1

```bash
./gnofacilitator -rpc https://rpc.topaz.testnets.gno.land:443 -chain-id topaz-1
```

It holds no keys — it only relays already-signed transactions, so it cannot redirect or re-price a
payment; what it does control is the seller's deliver-or-withhold decision (see
[Caveats](#caveats)). At startup it asks the node which chain it serves and **exits if that
contradicts `-chain-id`**: the sign doc covers the chain-id, so a mismatch would refuse every
payment as `signature_invalid` and blame payers for a typo. The check retries for 10s, because a
facilitator started alongside its node normally asks before the node is listening; after that it
warns and serves anyway — an unanswered query is not a mismatch. It listens on `:8402` by default
(`-listen` to change it) and logs:

```
level=INFO msg="gnofacilitator listening" addr=:8402 network=gno:topaz-1
```

## 3. Run the seller

```bash
./gnowars -rpc https://rpc.gno.land:443 -key mykey -network gno:topaz-1 \
  -facilitator http://localhost:8402
```

`-rpc` and `-key` are required: the seller reads the canvas realm to price an order and signs
the paid write itself. `-pay-to` defaults to the signing key's own address, which is what lets
each payment cover the write it triggers — so the seller needs no starting balance. That
holds because the quote is the ladder price PLUS the seller's own write cost: the realm
keeps none of the ladder amount (it forwards it to the displaced owner, or strands it at
its own address on a first claim), so a seller quoting the ladder alone would pay the fee
and the storage deposit out of its own pocket on every sale. `-storage-allowance` sizes
the deposit part, and it is per-chain.
`-network` defaults to `gno:topaz-1` and `-facilitator` to `http://localhost:8402`, so both can
be omitted if you're following this runbook exactly. It listens on `:8403` by default
(`-listen`) and logs:

```
level=INFO msg="gnowars listening" addr=:8403 network=gno:topaz-1 realm=gno.land/r/test/gnowars
  accepts="[gno:topaz-1 ugnot -> g1...]" usdcPerUgnot=0.000004
```

Settlements are decided against `-rpc` rather than on the facilitator's word. Use the same node
the facilitator broadcasts through where you can: the seller must read a chain view at least as
current as the facilitator's, and a payment that settles but is not seen inside the window
cannot be recovered.

`GET /` is the board; `GET /api/canvas.json` is the chain's own state of it; `POST /pixel` is the
priced resource. There is no `-amount`: the realm's ladder prices each cell — an unclaimed one
costs the base price, taking one costs twice what its owner paid — so the 402 quotes the order in
front of it. Add `-usdc-pay-to 0x...` to offer USDC on Base Sepolia alongside ugnot; a buyer
taking that option supplies a `payout` gno address on the order, which is what the canvas credits.

## 4. Authorize a session that can send

The master account needs topaz-1 GNOT before this step: it pays the `MsgCreateSession` fee
below, the 250000 ugnot payment itself, and the payment's own gas fee — fund it from the
topaz faucet, or use an existing funded key, before proposing the session.

In Claude Code (or any MCP client) with gnomcp connected, propose a session scoped to
`bank/send` on the built-in `testnet` profile (topaz-1). The built-in profile ships with no
`master-address`, so supply your own public address with `master_address` (or run `gnomcp
profile add` first with `--master` to persist it):

```
gno_session_propose(profile="testnet", allow_send=true, spend_limit="2000000ugnot", master_address="g1youraddresshere")
```

Size `spend_limit` to cover the payment amount plus a few writes' worth of gas fee — the
proposal hard-errors if the limit leaves nothing to pay with. A payment spends its amount on top
of its gas fee, so `allow_send` needs a limit strictly above one floor-gas write's fee: at or
below that, the only payment it could make is `0ugnot`. The tool prints a `gnokey maketx session
create` command; run it yourself from a machine holding your master key. gnomcp never sees that
key — this is the user-authorization step, and it's why the session (not the master account)
signs the payment.

## 5. Request the priced resource without paying

```bash
curl -i http://localhost:8403/premium
```

```
HTTP/1.1 402 Payment Required
Content-Type: application/json
PAYMENT-REQUIRED: eyJ4NDAyVmVyc2lvbiI6MiwiZXJyb3IiOiJQQVlNRU5ULVNJR05BVFVSRSBoZWFkZXIgaXMgcmVxdWlyZWQiLCJyZXNvdXJjZSI6eyJ1cmwiOiIvcHJlbWl1bSJ9LCJhY2NlcHRzIjpbeyJzY2hlbWUiOiJleGFjdCIsIm5ldHdvcmsiOiJnbm86dG9wYXotMSIsImFtb3VudCI6IjI1MDAwMCIsImFzc2V0IjoidWdub3QiLCJwYXlUbyI6ImcxeW91cmFkZHJlc3NoZXJlIiwibWF4VGltZW91dFNlY29uZHMiOjYwfV19

{
  "x402Version": 2,
  "error": "PAYMENT-SIGNATURE header is required",
  "resource": {
    "url": "/premium"
  },
  "accepts": [
    {
      "scheme": "exact",
      "network": "gno:topaz-1",
      "amount": "250000",
      "asset": "ugnot",
      "payTo": "g1youraddresshere",
      "maxTimeoutSeconds": 60
    }
  ]
}
```

The `PAYMENT-REQUIRED` header is the canonical carrier and decodes to exactly the body above;
the body is kept so the response stays readable in a terminal. HTTP header names are
case-insensitive, so Go writes it on the wire as `Payment-Required`.

## 6. Sign the payment

Take the `accepts[0]` object from step 5 (as a JSON string) and hand it to `gno_x402_pay`:

```
gno_x402_pay(profile="testnet", requirements="{\"scheme\":\"exact\",\"network\":\"gno:topaz-1\",\"amount\":\"250000\",\"asset\":\"ugnot\",\"payTo\":\"g1youraddresshere\",\"maxTimeoutSeconds\":60}")
```

This signs (but does not broadcast) a `bank/send` for exactly 250000 ugnot to `payTo` using the
session from step 4, and returns:

```
Signed by: session g1sessionaddr… on behalf of master g1youraddresshere

Signed a payment of 250000 ugnot to g1youraddresshere.

PAYMENT-SIGNATURE: <base64 header value>

curl example:
  curl -H "PAYMENT-SIGNATURE: <base64 header value>" <resource-url>

Caution: this signed payment stays valid until it settles or the session expires. It is bound
to the session's next sequence number, so it and any other transaction signed off that same
sequence are mutually exclusive: whichever settles first consumes the sequence and strands the
rest.
```

## 7. Retry with the payment

```bash
curl -H "PAYMENT-SIGNATURE: <base64 header value>" http://localhost:8403/premium
```

The seller forwards the payload to the facilitator's `/settle`, which broadcasts it; on
success the seller serves the resource and echoes the settlement in
`PAYMENT-RESPONSE` (base64 JSON, `{success, transaction, network, payer}`):

```
{"report":"the premium answer","paid":true}
```

## Caveats

- **`/verify` checks the signature; `/settle` is proof of payment.** The facilitator verifies a
  payment against its signer's on-chain account before it simulates anything, so a replayed or
  superseded payload is refused at `/verify` with
  `invalid_exact_gno_payload_sequence_mismatch` and a tampered one with
  `invalid_exact_gno_payload_signature_invalid`. The session's sequence still advances between
  the two calls, so sellers must treat a successful `/settle` response, not a successful
  `/verify`, as proof of payment. If the facilitator cannot reach the chain it answers `503`
  rather than a verdict — retry it; it is not a statement about the payment. The seller answers
  `503` on the same reasoning, for anything that leaves it without an outcome: a facilitator it
  could not reach or that answered unusably, a chain view that would not answer, or a payment
  another request is already settling. Retry the request with the same payment; do not pay again.
- **The facilitator bounds funds, not delivery.** Recipient and amount are inside the signed
  transaction, so the facilitator cannot redirect or re-price a payment. It does decide what the
  seller believes: the middleware serves the resource when `/settle` answers `success: true` and
  withholds it otherwise, over unauthenticated plaintext HTTP. A forged success hands out paid
  content for free; a forged failure on a payment that did broadcast takes the buyer's funds and
  withholds the content, looking exactly like a genuine settle failure. In this runbook that hop
  is loopback, which is what makes plain HTTP acceptable here. Off localhost the facilitator
  channel must be authenticated — TLS at minimum — and the facilitator itself trusted as a
  component; a seller with an RPC endpoint can also confirm a claimed settlement on chain.
- **A payment is a bearer credential.** The `/settle` body carries the complete signed transaction
  and nothing binds a payment to the request carrying it, so whoever can read that body can present
  the same `PAYMENT-SIGNATURE` header and receive the resource while the buyer gets a refusal. This
  is not the facilitator lying — the payment is genuine, only the requester is not the payer — so
  on-chain confirmation does not help. TLS off localhost closes the on-path case; the facilitator
  operator's own case is why it is trusted as a component. Binding `extra.memo` does not close it:
  a stolen credential carries the memo too.
- **This runbook takes the facilitator's word for it** unless you pass `-rpc`. On-chain
  confirmation is off by default — `PaymentConfig.Confirmer`, which decides a settlement against
  the seller's own node in both directions — which is acceptable here because the hop is loopback.
  Enabling it in a real deployment carries a requirement: the seller must read a chain view at
  least as current as the facilitator's. What it does and does not close is in
  [docs/adr/x402_scheme.md](../docs/adr/x402_scheme.md).
- **The facilitator throttles per remote address.** `/verify` and `/settle` allow 10 requests/s
  per peer with a burst of 20 (`-rate-per-second`, `-rate-burst`); `/supported` is not throttled.
  An IPv6 caller is keyed on its /64, since the holder of a routed /64 mints addresses inside it
  for free. `X-Forwarded-For` is deliberately ignored, so behind a reverse proxy every client
  shares the proxy's bucket — throttle upstream there. Expect `429 too many requests` if a load test outruns
  the bucket.
- **One outstanding payment per session.** A session's signed-but-unsettled payment is bound to
  its next sequence number, and anything else signed off that sequence is mutually exclusive with
  it: whichever settles first consumes the sequence and the rest fail to settle. Signing a second
  payment does not cancel the first. `gno_x402_pay` refuses a second payment while one is
  outstanding and hands that payment's header back, so retry the original resource with it rather
  than paying again.
- **If the facilitator dies mid-settle**, the broadcast may still have committed even though
  the seller never got a response — check the transaction hash on-chain before retrying the
  payment.
- Unaudited, experimental, GNOT-only. Interoperability with other x402 implementations is
  possible but untested: the transport is v2 (the three `PAYMENT-*` headers, the spec's status
  mapping and reason vocabulary, pinned against the spec's own fixtures), and what remains is
  publishing the gno `exact` scheme spec so a third party can implement the gno side.
