import {
  type Any,
  PubKeySecp256k1,
  Secp256k1PubKeyType,
  type Tx,
  type Wallet,
  encodeCharacterSet,
  sortedJsonStringify,
  stringToUTF8,
} from "@gnolang/tm2-js-client";

/** Expands encoded message values so the sign doc can sort them. */
export type DecodeTxMessages = (messages: Any[]) => unknown[];

/**
 * Signs a transaction against the chain it names, rather than the chain the
 * wallet happens to be connected to.
 *
 * Wallet.signTransaction takes the chain id from the node — it reads /status and
 * offers no override — so a buyer could only ever pay the chain their provider
 * already pointed at. An x402 offer names its chain, and that is the one a
 * payment has to be signed against, or one buyer cannot serve two chains.
 *
 * Avoiding /status has a second effect worth knowing: /status is the only
 * response that decodes a validator's bech32 address, which is the call that
 * forces a pinned @scure/base. Nothing on the account path decodes one.
 *
 * The sign doc is byte-identical to Wallet.signTransaction's for the same
 * inputs, and a test holds it that way. The account fields are the strings the
 * chain returned: they are quoted in the sign doc, so passing numbers would
 * change the bytes and invalidate every signature.
 */
export async function signForChain(
  wallet: Wallet,
  tx: Tx,
  chainId: string,
  decodeTxMessages: DecodeTxMessages,
): Promise<Tx> {
  const { fee } = tx;
  if (fee === undefined) {
    throw new Error("x402-gno: transaction names no fee, so nothing can be signed");
  }

  const address = await wallet.getAddress();
  const account = await wallet.getProvider().getAccount(address);

  const signBytes = stringToUTF8(
    encodeCharacterSet(
      sortedJsonStringify({
        chain_id: chainId,
        account_number: account.BaseAccount.account_number,
        sequence: account.BaseAccount.sequence,
        fee: {
          gas_fee: fee.gas_fee,
          gas_wanted: fee.gas_wanted.toString(10),
        },
        msgs: decodeTxMessages(tx.messages),
        memo: tx.memo,
      }),
    ),
  );

  const signer = wallet.getSigner();
  const publicKey = await signer.getPublicKey();

  return {
    ...tx,
    signatures: [
      ...tx.signatures,
      {
        pub_key: {
          type_url: Secp256k1PubKeyType,
          value: PubKeySecp256k1.encode({ key: publicKey }).finish(),
        },
        signature: await signer.signData(signBytes),
      },
    ],
  };
}
