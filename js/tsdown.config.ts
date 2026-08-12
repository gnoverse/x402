import { defineConfig } from "tsdown";

// tsc only checks — TypeScript 7's compiler is the type checker here and tsdown
// does the emit, the same split @gnolang/tm2-js-client uses.
//
// Dependencies and peers stay external: bundling them would ship a second copy of
// the wallet, and of the x402 client the consumer already has.
export default defineConfig({
  entry: ["src/mechanism.ts"],
  format: ["esm"],
  dts: true,
  clean: true,
});
