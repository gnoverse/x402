// Signs an x402 "exact" payment with the official gno JS SDK and prints the
// base64 transaction a foreign x402 client would carry in its payload.
//
// This is the buyer half of the interop check: the SDK an x402 client library
// would reach for, used the way it would use it, with nothing borrowed from the
// Go implementation the payment is verified by.
//
// Signing is offline. The SDK reads only the chain id from its provider, and the
// account number and sequence are supplied, so no node is contacted and the
// fixture is reproducible.
import { GnoWallet, MsgEndpoint, MsgSend, decodeTxMessages } from "@gnolang/gno-js-client";
import { Tx, uint8ArrayToBase64 } from "@gnolang/tm2-js-client";

const MNEMONIC =
  "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast";
const CHAIN_ID = "dev";
const PAY_TO = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq";
const AMOUNT = "250000ugnot";
const MEMO = "x402-interop";

const wallet = await GnoWallet.fromMnemonic(MNEMONIC);
wallet.connect({
  getStatus: async () => ({ node_info: { network: CHAIN_ID } }),
});

const from = await wallet.getAddress();

const tx = {
  messages: [
    {
      type_url: MsgEndpoint.MSG_SEND,
      value: MsgSend.encode({
        from_address: from,
        to_address: PAY_TO,
        amount: AMOUNT,
      }).finish(),
    },
  ],
  fee: { gas_wanted: 100000n, gas_fee: "1000000ugnot" },
  memo: MEMO,
  signatures: [],
};

const signed = await wallet.signTransaction(tx, decodeTxMessages, {
  accountNumber: "0",
  sequence: "0",
});

// The signer goes to stderr so stdout is the fixture and nothing else.
console.error(`signer: ${from}`);
console.log(uint8ArrayToBase64(Tx.encode(signed).finish()));
