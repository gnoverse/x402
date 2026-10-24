#!/usr/bin/env node
// Buys the weather from the seller in this directory: one paid GET, printed for
// a human. The x402 flow is the five numbered steps below: ask, read the terms
// from the 402, sign, ask again with the payment, read the receipt. @x402/core's
// stock client encodes and decodes each message; the gno-aware lines are the
// register() call, the spend control that admits ugnot, and the node the wallet
// connects to.
//
//   node buy.mjs [seller URL]
//   node buy.mjs --new-key [seller URL]
//
// The buyer signs with the mnemonic in X402_MNEMONIC — a funded account on the
// chain the seller's offer names — and reads its account sequence from a node on
// that chain: X402_GNO_RPC when set, else the chain's public RPC listed below,
// else a local node. With X402_SESSION_MASTER set to an account's address,
// X402_MNEMONIC is instead a session key that account registered, and payments
// move that account's coins. --new-key prints a fresh mnemonic and the command
// that pays the seller with it, and pays nothing.
import { readFileSync } from "node:fs";
import { relative } from "node:path";

import { Bip39 } from "@cosmjs/crypto";
import { GnoJSONRPCProvider, GnoWallet } from "@gnolang/gno-js-client";
import { generateEntropy } from "@gnolang/tm2-js-client";
import { x402Client, x402HTTPClient } from "@x402/core/client";
import { decodePaymentRequiredHeader, decodePaymentResponseHeader } from "@x402/core/http";

import { ExactGnoScheme } from "@gnoverse/x402-gno/exact/client";

// gno's well-known test1 mnemonic, the default for a local test chain.
const TEST1 =
  "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast";

const mnemonic = process.env.X402_MNEMONIC || TEST1;
const master = process.env.X402_SESSION_MASTER || undefined;

// The most this buyer pays for one request, in ugnot. The network fee is paid on
// top of it.
const MAX_PRICE = "1000000";

const sellerURL = process.argv[2] ?? process.env.X402_SELLER_URL ?? "http://localhost:8401/weather";

// The public RPC of each chain a buyer reads its account from when X402_GNO_RPC
// is unset. Any other chain, such as a local dev chain, falls back to LOCAL_RPC.
const PUBLIC_RPC = new Map([["onyx-1", "https://rpc.onyx.testnets.gno.land:443"]]);
const LOCAL_RPC = "http://127.0.0.1:26657";

// rpcFor returns the node to read the account from for a payment on network,
// "gno:<chain id>". A node reports its address in tm2's own form, which names
// the transport rather than the scheme an HTTP client needs.
function rpcFor(network) {
  const rpc = process.env.X402_GNO_RPC ?? PUBLIC_RPC.get(network.replace(/^gno:/, "")) ?? LOCAL_RPC;
  return rpc.replace(/^tcp:\/\//, "http://");
}

// --new-key writes nothing and sends no request: the mnemonic it prints is the
// key's only copy.
if (process.argv[2] === "--new-key") {
  // Drawn as @gnolang/tm2-js-client's Wallet.createRandom draws one: 32 random
  // bytes, 24 words. The address comes from the wallet a payment signs with.
  const fresh = Bip39.encode(generateEntropy()).toString();
  const freshAddress = await (await GnoWallet.fromMnemonic(fresh)).getAddress();
  // The pay command runs this file the way this run did: through npx, pinned to
  // the release tag of this version, or with node at this path.
  const { version } = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8"));
  const command =
    process.env.npm_lifecycle_event === "npx"
      ? `npx github:gnoverse/x402#client/v${version}`
      : `node ${relative(process.cwd(), process.argv[1])}`;
  // A URL with a character a shell reads, such as ? (a glob in zsh) or &, is
  // single-quoted, so the printed line pastes as it is.
  const seller = process.argv[3];
  const target =
    seller === undefined
      ? "<seller URL>"
      : /^[\w@%+=:,./-]+$/.test(seller)
        ? seller
        : `'${seller.replaceAll("'", `'\\''`)}'`;
  console.log(`a new key for the testnet, saved nowhere: this is the only copy of its mnemonic
use it as a throwaway, and never send it anything you would mind losing

mnemonic: ${fresh}
address:  ${freshAddress}

fund the address at https://faucet.gno.land, then pay with it:
  X402_MNEMONIC='${fresh}' ${command} ${target}`);
  process.exit(0);
}

// Prints why nothing was paid, then exits. fetch reports every network failure
// as "fetch failed" and keeps the reason in err.cause.
function notPaid(err) {
  const message = err instanceof Error ? err.message : String(err);
  const cause = err instanceof Error && err.cause instanceof Error ? ` (${err.cause.message})` : "";
  console.log(`not paid: ${message}${cause}`);
  process.exit(1);
}

const wallet = await GnoWallet.fromMnemonic(mnemonic);

// A stock client only pays in USD-pegged assets until told otherwise, and ugnot
// is not one. The wallet connects once the client has picked the offer it pays,
// to a node on the chain that offer names.
// The node the paying account is read from, once the client has picked an offer.
let node;
const client = new x402Client()
  .register("gno:*", new ExactGnoScheme(wallet, { master }))
  .setSpendControls({
    allowedAssets: [{ network: "gno:*", asset: "ugnot", maxAmountPerPayment: MAX_PRICE }],
  })
  .onBeforePaymentCreation(async ({ selectedRequirements }) => {
    node = rpcFor(selectedRequirements.network);
    wallet.connect(await GnoJSONRPCProvider.create(node));
  });
const http = new x402HTTPClient(client);

const address = await wallet.getAddress();
if (master !== undefined) {
  console.log(`buying as ${master}, signed by its session key ${address}`);
} else if (mnemonic === TEST1) {
  console.log(`buying as ${address}, gno's public test1 key; set X402_MNEMONIC to pay from your own account`);
} else {
  console.log(`buying as ${address}`);
}

let paid;
try {
  // 1. Ask without paying. A priced resource answers 402; any other answer is
  // the final one.
  paid = await fetch(sellerURL);
  if (paid.status === 402) {
    const unpaid = paid;
    // 2. Read the terms, which the 402 carries in its PAYMENT-REQUIRED header.
    const terms = http.getPaymentRequiredResponse((name) => unpaid.headers.get(name));
    // 3. Sign a payment that meets them: a bank send of the price to the payee,
    // within MAX_PRICE, over the account state the offer's chain reports.
    // Nothing is broadcast here.
    const payment = await client.createPaymentPayload(terms);
    // 4. Ask again with the signed payment in PAYMENT-SIGNATURE. The seller has
    // its facilitator check it and settle it on chain before answering. The
    // second header is one @x402/fetch adds to every payment, so a seller gets
    // the same request from this buyer as from that client.
    paid = await fetch(sellerURL, {
      headers: {
        ...http.encodePaymentSignatureHeader(payment),
        "Access-Control-Expose-Headers": "PAYMENT-RESPONSE,X-PAYMENT-RESPONSE",
      },
    });
  }
} catch (err) {
  // Thrown when the seller or the node cannot be reached, or no payment can be
  // signed, such as for an account the chain has never funded.
  // tm2-js-client marks an address with no account only by this message.
  notPaid(
    err instanceof Error && err.message === "account is not initialized"
      ? `${address} has no account at ${node}; fund it, then run this again (https://faucet.gno.land funds the public testnets)`
      : err,
  );
}

// 5. Read the answer: the weather once the payment settled, and the receipt in
// PAYMENT-RESPONSE.
const body = await paid.text();

if (!paid.ok) {
  // A verify refusal carries its reason in the challenge, a settle failure in the
  // receipt.
  const receiptHeader = paid.headers.get("PAYMENT-RESPONSE");
  const challenge = paid.headers.get("PAYMENT-REQUIRED");
  const receipt = receiptHeader ? decodePaymentResponseHeader(receiptHeader) : {};
  const reason = (receipt.errorReason || (challenge && decodePaymentRequiredHeader(challenge).error) || "").trim();
  if (receipt.errorReason === "settlement_pending") {
    // The chain admitted the payment and has not executed it yet: the weather is
    // withheld, but the payment may still go through, and the hash is how to
    // find out.
    console.log(
      `not confirmed yet: tx ${receipt.transaction} (${reason}); the weather was withheld, and the payment may still go through`,
    );
  } else if (receipt.errorReason === "duplicate_settlement") {
    // A copy of a payment the facilitator is settling or has settled: the copy
    // charges nothing, but the payment it copies may already have gone through.
    const tx = receipt.transaction ? `tx ${receipt.transaction} ` : "";
    console.log(
      `already settling: ${tx}(${reason}); the weather was withheld, and the payment may already have gone through`,
    );
  } else {
    // A refusal may name a transaction as well, such as one a block executed
    // and aborted, which still charged the network fee.
    const tx = receipt.transaction ? `; tx ${receipt.transaction}` : "";
    console.log(`not paid (status ${paid.status})${reason ? `: ${reason}` : ""}${tx}`);
  }
  process.exit(1);
}

const settlement = paid.headers.get("PAYMENT-RESPONSE");
if (settlement === null || settlement === "") {
  console.log("served without payment");
} else {
  const receipt = decodePaymentResponseHeader(settlement);
  console.log(`paid: tx ${receipt.transaction} by ${receipt.payer}`);
}
console.log(body.trim());
