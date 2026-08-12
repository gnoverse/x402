// The gno mechanism for the x402 "exact" scheme. An x402 client that has never
// heard of gno gains the ability to pay a gno chain by registering this — no fork
// of the client, no patch, no upstream change.
//
// It implements SchemeNetworkClient from @x402/core, which is a scheme name plus
// one method, and it does nothing else: no HTTP, no 402 handling, no
// broadcasting. The client library owns the protocol, the facilitator owns
// settlement, and this owns only the chain-specific step between them.
//
// ExactKeetaScheme in @x402/keeta is the in-tree analogue — same shape, one
// opaque base64 chain-native signed object in the payload.
import { MsgEndpoint, MsgSend, decodeTxMessages } from "@gnolang/gno-js-client";
import { Tx, type Wallet, uint8ArrayToBase64 } from "@gnolang/tm2-js-client";
import type {
  PaymentPayloadResult,
  PaymentRequirements,
  SchemeNetworkClient,
} from "@x402/core/types";

import { signForChain } from "./signing.js";

const NAMESPACE = "gno";
const ASSET = "ugnot";

// A gno buyer pays its own transaction fee: std.Fee names no payer and the ante
// charges the first signer, so the payer of the payment is also the payer of the
// fee. The requirements carry no gasWanted or gasFee, so the buyer chooses. This
// offers well clear of the chain's minimum rather than tracking the gas price,
// because an underpriced payment is refused at settle and costs the buyer the
// resource, while an overpriced one costs it only a little more than it had to.
const GAS_WANTED = 10_000_000n;
const GAS_FEE = "1000000ugnot";

/**
 * Returns the gno chain id a CAIP-2-style x402 network string names, or null when
 * the string does not name a gno chain.
 */
export function gnoChainId(network: string): string | null {
  const parts = network.split(":");
  if (parts.length !== 2) {
    return null;
  }
  const [namespace, chainId] = parts;
  if (namespace !== NAMESPACE || chainId === undefined || chainId === "") {
    return null;
  }
  return chainId;
}

/**
 * Reads extra.memo, the memo the requirements bind the payment's transaction to.
 *
 * An absent memo binds nothing. A memo that is present but not a string is a
 * malformed offer rather than an absent one, because signing "" against it would
 * produce a payment the seller refuses for a reason the buyer cannot see.
 */
function requiredMemo(extra: Record<string, unknown> | undefined): string {
  const memo = extra?.["memo"];
  if (memo === undefined) {
    return "";
  }
  if (typeof memo !== "string") {
    throw new Error(`x402-gno: extra.memo is ${typeof memo}, want string`);
  }
  return memo;
}

/**
 * Signs gno payments for the "exact" scheme. Register it against a gno network —
 * or the "gno:*" pattern for every gno chain — on an x402Client.
 */
export class ExactGnoScheme implements SchemeNetworkClient {
  readonly scheme = "exact";

  /**
   * @param wallet a GnoWallet connected to a provider. The connection is
   * required: a gno sequence is sequential, so only the chain knows the next one.
   * Sui and SVM buyers need chain access for their own reasons; only EVM's random
   * nonce lets a buyer sign entirely offline.
   */
  constructor(private readonly wallet: Wallet) {}

  /**
   * Builds the scheme payload: base64 of a fully signed, unbroadcast std.Tx
   * carrying a single bank.MsgSend.
   *
   * The requirements are validated rather than trusted — they arrive from a
   * seller over the network, and a malformed amount or a missing payTo would
   * otherwise become a signed transaction that pays the wrong thing.
   */
  async createPaymentPayload(
    x402Version: number,
    paymentRequirements: PaymentRequirements,
  ): Promise<PaymentPayloadResult> {
    const chainId = gnoChainId(paymentRequirements.network);
    if (chainId === null) {
      throw new Error(`x402-gno: ${paymentRequirements.network} does not name a gno chain`);
    }
    if (paymentRequirements.scheme !== this.scheme) {
      throw new Error(`x402-gno: scheme ${paymentRequirements.scheme} is not ${this.scheme}`);
    }
    if (paymentRequirements.asset !== ASSET) {
      throw new Error(`x402-gno: asset ${paymentRequirements.asset} is not ${ASSET}`);
    }
    if (!/^[1-9][0-9]*$/.test(paymentRequirements.amount)) {
      throw new Error(`x402-gno: amount ${paymentRequirements.amount} is not a positive integer`);
    }
    if (typeof paymentRequirements.payTo !== "string" || paymentRequirements.payTo === "") {
      throw new Error("x402-gno: requirements name no payTo");
    }

    const from = await this.wallet.getAddress();
    const tx: Tx = {
      messages: [
        {
          type_url: MsgEndpoint.MSG_SEND,
          value: MsgSend.encode({
            from_address: from,
            to_address: paymentRequirements.payTo,
            amount: `${paymentRequirements.amount}${paymentRequirements.asset}`,
          }).finish(),
        },
      ],
      fee: { gas_wanted: GAS_WANTED, gas_fee: GAS_FEE },
      memo: requiredMemo(paymentRequirements.extra),
      signatures: [],
    };

    // Signed against the chain the offer names, not the chain the wallet's node
    // happens to be. One buyer therefore pays any gno chain it is offered.
    const signed = await signForChain(this.wallet, tx, chainId, decodeTxMessages);

    return {
      x402Version,
      payload: { transaction: uint8ArrayToBase64(Tx.encode(signed).finish()) },
    };
  }
}
