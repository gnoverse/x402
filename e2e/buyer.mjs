// A stock x402 client buys from a gno seller. This is the buyer the payment test
// drives: pay_test.go hands it to the txtar scenarios as $X402_BUYER. Running it
// by hand against a live seller is the same path, with a real node.
//
// The only gno-aware line is the register() call. Everything the protocol
// requires — issuing the request, recognising the 402, decoding the offer,
// selecting an accepts[] entry, encoding PAYMENT-SIGNATURE, retrying — is
// @x402/fetch's own code, unmodified, exactly as it does for Base or Solana.
//
// Prints a JSON report on stdout describing what happened, so a caller checks the
// outcome rather than trusting an exit code. The scenarios assert on status,
// payer and transaction, so those field names are a contract. Diagnostics go to
// stderr.
import { GnoJSONRPCProvider, GnoWallet } from "@gnolang/gno-js-client";
import { decodePaymentResponseHeader, wrapFetchWithPayment, x402Client } from "@x402/fetch";

// Imported by package name, not by path: this buyer goes through the same
// entrypoint a stranger installing the package would, so a broken exports map
// fails here rather than after publishing.
import { ExactGnoScheme } from "@gnoverse/x402-gno/exact/client";

const PAYMENT_RESPONSE_HEADER = "PAYMENT-RESPONSE";

// The well-known test1 key. This buyer only ever pays on throwaway chains.
const MNEMONIC =
  "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast";

function fromEnv(name) {
  const value = process.env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} must be set`);
  }
  return value;
}

// The node reports its address in tm2's own form, which names the transport
// rather than the scheme an HTTP client needs.
function httpURL(addr) {
  return addr.replace(/^tcp:\/\//, "http://");
}

const sellerURL = fromEnv("X402_SELLER_URL");
const rpcURL = httpURL(fromEnv("X402_GNO_RPC"));

const wallet = await GnoWallet.fromMnemonic(MNEMONIC);
// create is the provider's public factory; its constructor is protected, and
// reaching a transport client for it would mean depending on a package this
// buyer only ever resolved by hoisting.
wallet.connect(await GnoJSONRPCProvider.create(rpcURL));

// Teach a stock client about gno. `register` validates nothing about the network
// string — there is no chain allowlist — and lookup is glob-matched, so "gno:*"
// covers every gno chain at once.
const client = new x402Client().register("gno:*", new ExactGnoScheme(wallet));
const fetchWithPayment = wrapFetchWithPayment(fetch, client);

console.error(`buying as ${await wallet.getAddress()}`);

// One call. The refusal and the payment happen inside the client.
//
// The resource decides the method and nothing about x402 is method-specific, so a
// paid GET is the ordinary case — the weather seller is one. Override for a
// resource that wants something else.
const paid = await fetchWithPayment(sellerURL, { method: process.env.X402_METHOD ?? "GET" });
const body = await paid.text();

// A settlement report is expected on success and is what names the payer and the
// transaction; a refusal may legitimately carry none, and the report says so
// rather than failing here, because the caller checks the status.
let settle = {};
const settleHeader = paid.headers.get(PAYMENT_RESPONSE_HEADER);
if (settleHeader !== null && settleHeader !== "") {
  settle = decodePaymentResponseHeader(settleHeader);
}
console.error(`status: ${paid.status} settled: ${settle.success === true}`);

process.stdout.write(
  JSON.stringify({
    status: paid.status,
    body,
    payer: settle.payer ?? "",
    transaction: settle.transaction ?? "",
  }),
);
