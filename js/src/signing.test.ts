import { MsgEndpoint, MsgSend, decodeTxMessages } from "@gnolang/gno-js-client";
import {
  type ABCIAccount,
  type Provider,
  type Status,
  Tx,
  Wallet,
  uint8ArrayToBase64,
} from "@gnolang/tm2-js-client";
import { describe, expect, it } from "vitest";

import { signForChain } from "./signing.js";

// The well-known test1 account. Nothing is broadcast, so the key only has to be
// the same one on both sides of the comparison.
const MNEMONIC =
  "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast";

const REPORTED_CHAIN = "dev";
const ACCOUNT_NUMBER = "12";
const SEQUENCE = "7";
const PAY_TO = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq";

// A provider that answers the two questions signing asks and refuses the rest,
// so a call that reaches the network in a signing path fails the test loudly.
class FakeProvider implements Provider {
  constructor(private readonly reportedChain: string) {}

  async getStatus(): Promise<Status> {
    return {
      node_info: {
        version_set: [],
        net_address: "",
        network: this.reportedChain,
        software: "",
        version: "",
        channels: "",
        monkier: "",
        other: { tx_index: "", rpc_address: "" },
      },
      sync_info: {
        latest_block_hash: "",
        latest_app_hash: "",
        latest_block_height: "1",
        latest_block_time: "",
        catching_up: false,
      },
      validator_info: {
        address: "",
        pub_key: { type: "", value: "" },
        voting_power: "0",
      },
    };
  }

  async getAccount(address: string): Promise<ABCIAccount> {
    return {
      BaseAccount: {
        address,
        coins: "1000000000ugnot",
        public_key: null,
        account_number: ACCOUNT_NUMBER,
        sequence: SEQUENCE,
      },
    };
  }

  private unreachable(): never {
    throw new Error("signing must not reach this provider method");
  }

  getBalance(): Promise<number> {
    return this.unreachable();
  }
  getAccountSequence(): Promise<number> {
    return this.unreachable();
  }
  getAccountNumber(): Promise<number> {
    return this.unreachable();
  }
  getBlock(): never {
    return this.unreachable();
  }
  getBlockResult(): never {
    return this.unreachable();
  }
  getBlockNumber(): Promise<number> {
    return this.unreachable();
  }
  getNetwork(): never {
    return this.unreachable();
  }
  getConsensusParams(): never {
    return this.unreachable();
  }
  getGasPrice(): Promise<number> {
    return this.unreachable();
  }
  estimateGas(): Promise<bigint> {
    return this.unreachable();
  }
  getTransaction(): never {
    return this.unreachable();
  }
  sendTransaction(): never {
    return this.unreachable();
  }
  waitForTransaction(): Promise<Tx> {
    return this.unreachable();
  }
}

async function connectedWallet(): Promise<Wallet> {
  const wallet = await Wallet.fromMnemonic(MNEMONIC);
  wallet.connect(new FakeProvider(REPORTED_CHAIN));
  return wallet;
}

function paymentTx(from: string): Tx {
  return {
    messages: [
      {
        type_url: MsgEndpoint.MSG_SEND,
        value: MsgSend.encode({
          from_address: from,
          to_address: PAY_TO,
          amount: "250000ugnot",
        }).finish(),
      },
    ],
    fee: { gas_wanted: 10_000_000n, gas_fee: "1000000ugnot" },
    memo: "x402-interop",
    signatures: [],
  };
}

function encoded(tx: Tx): string {
  return uint8ArrayToBase64(Tx.encode(tx).finish());
}

describe("signForChain", () => {
  // The safety net. This reimplements the wallet's sign doc so the chain id can
  // come from the offer instead of the node, and a sign doc that differs by one
  // byte makes every payment invalid while looking like a facilitator bug.
  it("produces the same bytes as the wallet's own signing", async () => {
    const wallet = await connectedWallet();
    const from = await wallet.getAddress();

    const ours = await signForChain(wallet, paymentTx(from), REPORTED_CHAIN, decodeTxMessages);
    const theirs = await wallet.signTransaction(paymentTx(from), decodeTxMessages);

    expect(encoded(ours)).toBe(encoded(theirs));
  });

  // What the whole change is for: one buyer can pay any gno chain named in an
  // offer, not only the chain their provider happens to point at.
  it("signs against the chain it is given rather than the one the node reports", async () => {
    const wallet = await connectedWallet();
    const from = await wallet.getAddress();

    const elsewhere = await signForChain(wallet, paymentTx(from), "test14", decodeTxMessages);
    const reported = await signForChain(wallet, paymentTx(from), REPORTED_CHAIN, decodeTxMessages);

    expect(encoded(elsewhere)).not.toBe(encoded(reported));
  });

  it("refuses a transaction that names no fee", async () => {
    const wallet = await connectedWallet();
    const from = await wallet.getAddress();
    const { fee: _dropped, ...feeless } = paymentTx(from);

    await expect(signForChain(wallet, feeless, REPORTED_CHAIN, decodeTxMessages)).rejects.toThrow(
      /names no fee/,
    );
  });
});
