// Checks the seller's advertised EIP-712 domain against the token contract itself.
//
// An EIP-3009 authorization is signed over a domain built from the offer's extra.name,
// extra.version, the network's chain id and the asset address. Get any of them wrong and
// the signature is simply not the one the contract recomputes — it reverts with nothing
// that names the cause, after a buyer has spent real balance getting there. So the domain
// the seller publishes is compared against the contract's own DOMAIN_SEPARATOR().
//
// Read-only: one eth_call, no key, no funds, nothing signed. Prints JSON on stdout.
import { createPublicClient, hashDomain, http } from "viem";

function fromEnv(name) {
  const value = process.env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} must be set`);
  }
  return value;
}

const rpcURL = fromEnv("BASE_SEPOLIA_RPC");
const asset = fromEnv("USDC_ASSET");
const name = fromEnv("USDC_NAME");
const version = fromEnv("USDC_VERSION");
const chainId = Number(fromEnv("USDC_CHAIN_ID"));
if (!Number.isInteger(chainId)) {
  throw new Error(`USDC_CHAIN_ID must be an integer, got ${process.env.USDC_CHAIN_ID}`);
}

const local = hashDomain({
  domain: { name, version, chainId, verifyingContract: asset },
  types: {
    EIP712Domain: [
      { name: "name", type: "string" },
      { name: "version", type: "string" },
      { name: "chainId", type: "uint256" },
      { name: "verifyingContract", type: "address" },
    ],
  },
});

const client = createPublicClient({ transport: http(rpcURL) });
const onchain = await client.readContract({
  address: asset,
  abi: [
    {
      name: "DOMAIN_SEPARATOR",
      type: "function",
      stateMutability: "view",
      inputs: [],
      outputs: [{ type: "bytes32" }],
    },
  ],
  functionName: "DOMAIN_SEPARATOR",
});

// The contract's own name and version, so a mismatch says WHICH field is wrong rather than
// only that the hashes differ.
const [onchainName, onchainVersion] = await Promise.all(
  ["name", "version"].map((fn) =>
    client.readContract({
      address: asset,
      abi: [{ name: fn, type: "function", stateMutability: "view", inputs: [], outputs: [{ type: "string" }] }],
      functionName: fn,
    }),
  ),
);

process.stdout.write(
  JSON.stringify({
    match: local.toLowerCase() === onchain.toLowerCase(),
    local,
    onchain,
    onchainName,
    onchainVersion,
  }),
);
