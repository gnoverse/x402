// A buyer holding only testnet USDC paints a pixel on a gno.land realm.
//
// There is no gno code here and no gno key. The only thing this buyer knows about gno
// is the payout address it was handed — an address somebody generated and never funded,
// so the canvas has a gno identity to credit the pixel to. It holds no GNOT, has never
// signed a gno transaction, and could not reach this board any other way.
//
// It holds no ETH either. The EIP-3009 flow signs an EIP-712 *authorization*, not a
// transaction: the facilitator submits transferWithAuthorization and pays the gas. That
// path is the default whenever the seller's offer carries no extra.assetTransferMethod,
// which is why ExactEvmScheme's second `options` argument is deliberately absent — it
// only backfills the Permit2/EIP-2612 extensions, which this flow never reaches.
//
// Every protocol step — issuing the request, recognising the 402, choosing among the
// accepts[] entries, signing, encoding PAYMENT-SIGNATURE, retrying — is the stock
// library's own code. Nothing in this file is gno-aware or x402-aware.
import { registerExactEvmScheme } from "@x402/evm/exact/client";
import { decodePaymentResponseHeader, wrapFetchWithPayment, x402Client } from "@x402/fetch";
import { privateKeyToAccount } from "viem/accounts";

const PAYMENT_RESPONSE_HEADER = "PAYMENT-RESPONSE";

function fromEnv(name) {
  const value = process.env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} must be set`);
  }
  return value;
}

// One cell per run, given as "x,y,color". It is configurable rather than fixed because
// the board charges twice what a cell's owner paid: re-running against the same cell
// costs double every time, which reads as a pricing bug rather than the ladder working.
function pixelFromEnv() {
  const raw = process.env.GNOWARS_PIXEL ?? "12,7,red";
  const [x, y, color] = raw.split(",").map((part) => part.trim());
  if (!/^\d+$/.test(x) || !/^\d+$/.test(y) || !color) {
    throw new Error(`GNOWARS_PIXEL must look like "12,7,red", got ${JSON.stringify(raw)}`);
  }
  return { x: Number(x), y: Number(y), color };
}

const signer = privateKeyToAccount(fromEnv("EVM_PRIVATE_KEY"));
const sellerURL = fromEnv("X402_SELLER_URL");
const payout = fromEnv("GNO_PAYOUT_ADDRESS");
const pixel = pixelFromEnv();

// Teach a stock client about EVM. The helper is the library's own: it registers the
// scheme against "eip155:*", so any EVM chain the seller offers is covered.
const client = new x402Client();
registerExactEvmScheme(client, { signer });
const fetchWithPayment = wrapFetchWithPayment(fetch, client);

console.error(`paying as ${signer.address}, crediting ${payout}`);
console.error(`painting ${pixel.x},${pixel.y} ${pixel.color}`);

// One call. The refusal, the choice of asset, and the payment all happen inside it.
const paid = await fetchWithPayment(sellerURL, {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ payout, pixels: [pixel] }),
});
const body = await paid.text();

// A settlement report is expected on success and is what names the payer and the
// transaction. A refusal may legitimately carry none, so its absence is reported rather
// than thrown: the caller decides from the status.
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
    buyer: signer.address,
    payout,
    payer: settle.payer ?? "",
    network: settle.network ?? "",
    transaction: settle.transaction ?? "",
  }),
);
