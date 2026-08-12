# @gnoverse/x402-gno

Pay a gno.land chain with x402. One line on a stock client — no fork, no patch, no upstream change.

> [!NOTE]
> **Not published to a registry yet.** Build it from the repository root (`make js`)
> and reference it by path, or use the buyer in `buy.mjs` as-is. The install below is what it will
> be, not what it is.

```sh
npm install @gnoverse/x402-gno
```

```js
import { x402Client } from "@x402/core/client";
import { wrapFetchWithPayment } from "@x402/fetch";
import { ExactGnoScheme } from "@gnoverse/x402-gno/exact/client";

const client = new x402Client();
client.register("gno:*", new ExactGnoScheme(wallet)); // ← the only gno-aware line

const paid = wrapFetchWithPayment(fetch, client);
const res = await paid("https://api.example.com/weather");
```

`wallet` is a `GnoWallet` from `@gnolang/gno-js-client`, connected to a provider. The connection is
required: a gno sequence is sequential, so only the chain knows the next one.

## What it is

`ExactGnoScheme` implements `SchemeNetworkClient` from `@x402/core` — a scheme name plus one method.
It does nothing else: no HTTP, no 402 handling, no broadcasting. The client library owns the
protocol, the facilitator owns settlement, and this owns only the chain-specific step between them.

The subpath is the ecosystem's grid: `@x402/evm` publishes `./exact/client`, `./exact/server` and
`./exact/facilitator`, so a buyer that already imports one reaches the same way for this. gno's
server and facilitator halves are Go, so `./exact/client` is the only cell this package fills.

Its payload is one field, `transaction`: base64 of a fully signed, unbroadcast `std.Tx` carrying a
single `bank.MsgSend`.

## The chain id comes from the offer

The payment is signed against the chain id the **offer** names, not the one the wallet's node
reports. That is worth stating because the obvious implementation cannot do it:
`Wallet.signTransaction` takes the chain id from the node it is connected to and offers no override.
This builds the sign doc itself instead, and a test holds the result byte-identical to the wallet's
own signing.

The chain id is the only sign-doc field that comes from the offer. The account number and the
sequence still come from the wallet's provider, because only a chain knows the next sequence for an
account — so paying a second gno chain also means pointing the wallet at a node on it. A wallet
connected elsewhere signs over that node's account state, and the facilitator verifying against the
paid chain refuses the result as `invalid_exact_gno_payload_signature_invalid`.

## The fee is yours

gno has no fee delegation and this scheme does not pretend otherwise. `std.Fee` names no payer and
the chain charges the first signer, so the payer of the payment is also the payer of the network
fee, and the facilitator holds no key — it only broadcasts what you signed. A gno offer says so:
`extra.areFeesSponsored` is `false`.

The requirements carry no gas fields, so the buyer chooses. This offers well clear of the chain's
minimum rather than tracking the gas price: an underpriced payment is refused at settle and costs
you the resource, while an overpriced one costs a little more than it had to.

## What it refuses

The requirements arrive from a seller over the network, so they are validated rather than trusted —
a malformed amount or a missing `payTo` would otherwise become a signed transaction that pays the
wrong thing. It throws on a network that is not `gno:<chain-id>`, a scheme that is not `exact`, an
asset that is not `ugnot`, an amount that is not a positive integer, an absent `payTo`, and an
`extra.memo` that is present but not a string.

## Development

Sources are here in `js/src`, laid out by the subpath they publish; the manifest and the emit are at
the repository root, so these run from there.

```sh
npm run typecheck   # TypeScript 7 (the native compiler)
npm test            # vitest
npm run build       # typecheck, then emit js/dist/
```
