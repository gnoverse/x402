import { defineConfig } from "tsdown";

// tsc only checks — TypeScript 7's compiler is the type checker here and tsdown
// does the emit, the same split @gnolang/tm2-js-client uses.
//
// Dependencies and peers stay external: bundling them would ship a second copy of
// the wallet, and of the x402 client the consumer already has.
// The emit lands under js/, beside the sources it is built from, because the
// repository root's dist/ belongs to goreleaser: it writes artifacts.json there
// and CI reads it, while `clean: true` below empties whatever directory this
// names. Two tools owning one directory means each run destroys the other's
// output, and a missing client is a skipped payment test rather than a failure.
export default defineConfig({
  entry: ["js/src/exact/client.ts"],
  outDir: "js/dist",
  format: ["esm"],
  dts: true,
  clean: true,
});
