import { Wallet } from "@gnolang/tm2-js-client";
import type { PaymentRequirements } from "@x402/core/types";
import { describe, expect, it } from "vitest";

import { ExactGnoScheme, gnoChainId } from "./client.js";

// The well-known test1 account. Nothing is signed or broadcast in these tests —
// every case here is refused before the wallet's key is used for anything.
const MNEMONIC =
  "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast";

const PAY_TO = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq";

function offer(overrides: Partial<PaymentRequirements> = {}): PaymentRequirements {
  return {
    scheme: "exact",
    network: "gno:dev",
    amount: "250000",
    asset: "ugnot",
    payTo: PAY_TO,
    maxTimeoutSeconds: 60,
    extra: {},
    ...overrides,
  };
}

// The wallet is deliberately left unconnected. Every case below must be refused
// by the mechanism's own validation, so none of them may reach a provider — and
// if one did, the error would name the missing connection instead of the offer,
// which is what asserting the message rules out.
async function scheme(): Promise<ExactGnoScheme> {
  return new ExactGnoScheme(await Wallet.fromMnemonic(MNEMONIC));
}

describe("gnoChainId", () => {
  // The CAIP-2 two-part rule. This is the only implementation of it in the
  // repository — the Go half concatenates "gno:" onto a configured chain id and
  // string-compares the result, so nothing there splits a network string.
  const chains: Record<string, string | null> = {
    "gno:dev": "dev",
    "gno:test14": "test14",
    "gno:portal-loop": "portal-loop",
    // A chain id holding a colon splits into three, and CAIP-2 names exactly two
    // parts. Accepting it would make one network string mean two different offers.
    "gno:test:14": null,
    "gno:": null,
    gno: null,
    // Another chain's namespace, and one that merely starts the same way.
    "eip155:8453": null,
    "gnoland:dev": null,
    "": null,
  };

  for (const [network, want] of Object.entries(chains)) {
    it(`reads ${JSON.stringify(network)} as ${JSON.stringify(want)}`, () => {
      expect(gnoChainId(network)).toBe(want);
    });
  }
});

describe("ExactGnoScheme.createPaymentPayload", () => {
  // Each refusal has its own message because the buyer is the only party that
  // can act on it: the seller published the offer and the facilitator never sees
  // the payment, so a signed transaction against a malformed offer would be
  // refused later for a reason naming the payload rather than the offer.
  const refusals: Record<string, { requirements: PaymentRequirements; message: RegExp }> = {
    "a network that names no gno chain": {
      requirements: offer({ network: "eip155:8453" }),
      message: /does not name a gno chain/,
    },
    "a chain id that is not one CAIP-2 part": {
      requirements: offer({ network: "gno:test:14" }),
      message: /does not name a gno chain/,
    },
    "another scheme": {
      requirements: offer({ scheme: "upto" }),
      message: /scheme upto is not exact/,
    },
    "another asset": {
      requirements: offer({ asset: "atom" }),
      message: /asset atom is not ugnot/,
    },
    "a dollar-denominated amount": {
      requirements: offer({ amount: "$0.01" }),
      message: /is not a positive integer/,
    },
    "a zero amount": {
      requirements: offer({ amount: "0" }),
      message: /is not a positive integer/,
    },
    "a negative amount": {
      requirements: offer({ amount: "-250000" }),
      message: /is not a positive integer/,
    },
    "a fractional amount": {
      requirements: offer({ amount: "0.25" }),
      message: /is not a positive integer/,
    },
    "an empty amount": {
      requirements: offer({ amount: "" }),
      message: /is not a positive integer/,
    },
    "an empty payTo": {
      requirements: offer({ payTo: "" }),
      message: /no payTo/,
    },
  };

  for (const [name, { requirements, message }] of Object.entries(refusals)) {
    it(`refuses ${name}`, async () => {
      const mechanism = await scheme();
      await expect(mechanism.createPaymentPayload(2, requirements)).rejects.toThrow(message);
    });
  }

  // A memo that is present but not a string is a malformed offer, not an absent
  // one: signing "" against it produces a payment the seller refuses for a memo
  // mismatch, which the buyer cannot diagnose from its own side.
  it("refuses a non-string extra.memo", async () => {
    const mechanism = await scheme();
    await expect(
      mechanism.createPaymentPayload(2, offer({ extra: { memo: 42 } })),
    ).rejects.toThrow(/extra\.memo is number, want string/);
  });

  // An absent memo binds nothing and must not be confused with a malformed one,
  // so it has to get past validation. Reaching signing is the proof: signing is
  // the first step that needs the provider this wallet does not have, so that is
  // the failure a well-formed offer produces here.
  it("accepts an absent extra.memo and reaches signing", async () => {
    const mechanism = await scheme();
    await expect(mechanism.createPaymentPayload(2, offer())).rejects.toThrow(
      /no provider connected/,
    );
  });
});
