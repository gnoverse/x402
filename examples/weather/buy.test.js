import { execFile } from "node:child_process";
import { mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { GnoWallet } from "@gnolang/gno-js-client";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { shellWord } from "./paywall.js";

const BUYER = new URL("./buy.mjs", import.meta.url);
const REPOSITORY = fileURLToPath(new URL("../../", import.meta.url));
const MANIFEST = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8"));

// The package a bare import specifier resolves into: "@scope/name/sub" is in
// "@scope/name", "name/sub" in "name".
function packageOf(specifier) {
  const parts = specifier.split("/");
  return specifier.startsWith("@") ? parts.slice(0, 2).join("/") : parts[0];
}

describe("buy.mjs, run as the package's x402-gno command", () => {
  // npx and npm install the package with its dependencies and peers only, so
  // importing a devDependency works in the repository and fails for everyone
  // running the command.
  it("imports only what installing the package brings", () => {
    const source = readFileSync(BUYER, "utf8");
    const imported = [...source.matchAll(/\bfrom\s+"([^"]+)"/g)]
      .map(([, specifier]) => specifier)
      .filter((specifier) => !specifier.startsWith("node:") && !specifier.startsWith("."));
    const installed = new Set([
      MANIFEST.name,
      ...Object.keys(MANIFEST.dependencies),
      ...Object.keys(MANIFEST.peerDependencies),
    ]);

    expect(imported.length).toBeGreaterThan(0);
    expect(imported.map(packageOf).filter((name) => !installed.has(name))).toEqual([]);
  });
});

describe("buy.mjs paying", () => {
  let seller;
  let node;
  let seen;

  // An offer for a chain whose public RPC the buyer knows, so a buyer that
  // reaches the node at X402_GNO_RPC did not fall back to its own table.
  const terms = {
    x402Version: 2,
    error: "Payment required",
    resource: { url: "http://seller.test/weather", description: "Weather data", mimeType: "application/json" },
    accepts: [
      {
        scheme: "exact",
        network: "gno:onyx-1",
        asset: "ugnot",
        amount: "250000",
        payTo: "g1nmzz8jc66mgfswfqhuze5ulmyde3ql5hsyffud",
        maxTimeoutSeconds: 300,
        extra: { areFeesSponsored: false },
      },
    ],
  };

  // The seller answers every request with the offer. The node answers nothing
  // the wallet can use, so the buyer stops once it has asked it.
  beforeEach(async () => {
    seen = [];
    seller = createServer((_req, res) => {
      seen.push("seller");
      res.writeHead(402, { "PAYMENT-REQUIRED": Buffer.from(JSON.stringify(terms)).toString("base64") });
      res.end("{}");
    });
    node = createServer((_req, res) => {
      seen.push("node");
      res.writeHead(500);
      res.end();
    });
    await Promise.all([seller, node].map((server) => new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))));
  });

  afterEach(async () => {
    await Promise.all([seller, node].map((server) => new Promise((resolve) => server.close(resolve))));
  });

  // Runs the buyer against the seller with X402_GNO_RPC naming the node, and
  // waits for it to exit.
  async function buy() {
    await new Promise((resolve) => {
      execFile(
        process.execPath,
        [fileURLToPath(BUYER), `http://127.0.0.1:${seller.address().port}/weather`],
        { env: { PATH: process.env.PATH, X402_GNO_RPC: `http://127.0.0.1:${node.address().port}` } },
        resolve,
      );
    });
  }

  // The offer names the chain, and so the node the payment's account state must
  // come from.
  it("reads the offer before it asks a node anything", async () => {
    await buy();

    expect(seen[0]).toBe("seller");
  });

  it("reads the account from X402_GNO_RPC when it is set, whatever chain the offer names", async () => {
    await buy();

    expect(seen).toContain("node");
  });
});

describe("buy.mjs --new-key", () => {
  let home;
  let node;
  let requests;

  // Any request the buyer makes lands here: it is both the chain's RPC and the
  // seller, so a key made without touching the network leaves it at zero.
  beforeEach(async () => {
    home = mkdtempSync(join(tmpdir(), "buy-new-key-"));
    requests = 0;
    node = createServer((_req, res) => {
      requests += 1;
      res.end();
    });
    await new Promise((resolve) => node.listen(0, "127.0.0.1", resolve));
  });

  afterEach(async () => {
    await new Promise((resolve) => node.close(resolve));
    rmSync(home, { recursive: true, force: true });
  });

  // The seller URL a test hands --new-key: the node, so asking it counts.
  function sellerURL() {
    return `http://127.0.0.1:${node.address().port}/weather`;
  }

  // Runs buy.mjs --new-key with args after it, in cwd, an empty directory that
  // is also its HOME unless named, with env added to a clean environment, and
  // returns what it printed.
  async function newKey({ args = [], env = {}, cwd = home } = {}) {
    const url = `http://127.0.0.1:${node.address().port}`;
    const { stdout } = await new Promise((resolve, reject) => {
      execFile(
        process.execPath,
        [fileURLToPath(BUYER), "--new-key", ...args],
        {
          cwd,
          env: { PATH: process.env.PATH, HOME: home, X402_GNO_RPC: url, X402_SELLER_URL: `${url}/weather`, ...env },
        },
        (err, out, errOut) => (err ? reject(err) : resolve({ stdout: out, stderr: errOut })),
      );
    });
    const mnemonic = stdout.match(/^mnemonic: (.+)$/m)?.[1];
    const address = stdout.match(/^address: +(\S+)$/m)?.[1];
    const pay = stdout.match(/^ +(X402_MNEMONIC=.+)$/m)?.[1];
    return { stdout, mnemonic, address, pay };
  }

  it("prints a 24-word mnemonic and the address a payment signed with it comes from", async () => {
    const { mnemonic, address } = await newKey();

    expect(mnemonic?.split(" ")).toHaveLength(24);
    const wallet = await GnoWallet.fromMnemonic(mnemonic);
    expect(address).toBe(await wallet.getAddress());
  });

  it("makes a different key every time", async () => {
    const first = await newKey();
    const second = await newKey();

    expect(first.mnemonic).not.toBe(second.mnemonic);
  });

  it("says the key is a testnet throwaway kept nowhere, and where to fund it", async () => {
    const { stdout } = await newKey();

    expect(stdout).toMatch(/testnet/);
    expect(stdout).toMatch(/throwaway/);
    expect(stdout).toMatch(/saved nowhere/);
    expect(stdout).toContain("https://faucet.gno.land");
  });

  it("prints the command that pays the seller it was given with the key, run the way it was run", async () => {
    const { mnemonic, pay } = await newKey({ args: ["https://weather.example/weather"], cwd: REPOSITORY });

    expect(pay).toBe(`X402_MNEMONIC='${mnemonic}' node examples/weather/buy.mjs https://weather.example/weather`);
  });

  it("prints the pinned npx command when npx ran it", async () => {
    const { mnemonic, pay } = await newKey({
      args: ["https://weather.example/weather"],
      env: { npm_lifecycle_event: "npx" },
    });

    expect(pay).toBe(
      `X402_MNEMONIC='${mnemonic}' npx github:gnoverse/x402#client/v${MANIFEST.version} https://weather.example/weather`,
    );
  });

  it("leaves the seller to fill in when it was given none", async () => {
    const { mnemonic, pay } = await newKey({ cwd: REPOSITORY });

    expect(pay).toBe(`X402_MNEMONIC='${mnemonic}' node examples/weather/buy.mjs <seller URL>`);
  });

  // An unquoted ? is a glob in zsh and & ends the command in every shell.
  it("quotes a seller URL the shell would otherwise read", async () => {
    const { mnemonic, pay } = await newKey({ args: ["https://weather.example/weather?city=paris&units=c"], cwd: REPOSITORY });

    expect(pay).toBe(
      `X402_MNEMONIC='${mnemonic}' node examples/weather/buy.mjs 'https://weather.example/weather?city=paris&units=c'`,
    );
  });

  // The weather page shows the pay command this prints, so both write a URL as
  // the same shell word.
  it("prints the seller URL as the weather page writes it", async () => {
    for (const seller of [
      "https://weather.example/weather",
      "https://weather.example/weather?city=paris&units=c",
      "https://weather.example/o'clock/weather",
    ]) {
      const { mnemonic, pay } = await newKey({ args: [seller], cwd: REPOSITORY });

      expect(pay).toBe(`X402_MNEMONIC='${mnemonic}' node examples/weather/buy.mjs ${shellWord(seller)}`);
    }
  });

  it("touches neither the network nor the disk", async () => {
    await newKey({ args: [sellerURL()] });

    expect(requests).toBe(0);
    expect(readdirSync(home)).toEqual([]);
  });
});
