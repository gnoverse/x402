# AGENTS.md — working on this repo

This file is for agents **contributing to gnoverse/x402**: the gno payment mechanism for
x402, its facilitator, and the harnesses that prove a payment happens. If you're here to
*use* the mechanism, the READMEs are the entry point.

## Commands

```bash
make test                # library tests. No network, no chain, no npm
make js-test             # the client mechanism, incl. the sign-doc byte-equality check
make test-e2e            # one real payment against an in-memory node
make lint                # go vet (both modules) + gofmt
make build               # bin/gnofacilitator
make js                  # install and build the client bundle
make help                # every target
```

## Map

The top level is the x402 role grid — server, facilitator, client — and no directory
names a language.

| Path | What |
|---|---|
| `facilitator/` | verification, settlement, the `/verify` `/settle` `/supported` service, and the wire types + reason vocabulary the other roles import |
| `server/exact/` | upstream's `SchemeNetworkServer` implemented for gno (package `exact`) |
| `client/src/` | upstream's `SchemeNetworkClient`, TypeScript; emits to `client/dist/` |
| `cmd/gnofacilitator/` | the binary |
| `examples/weather/` | a priced endpoint you can run and curl |
| `e2e/` | one real payment through a real node — its own Go module. `buyer.mjs` beside it is the buyer the scenarios drive |
| `docs/releasing.md` | the two independent release lines |

`e2e` is a separate Go module on purpose, so `go test ./...` cannot start a chain by
accident and needs no build tag to stay out of the way.

## Conventions

- TDD: failing test first. Unit tests are Go-native; the payment claim is proved by the
  e2e, not by mocks.
- testify `require`/`assert`; small focused test funcs over mega-tables.
- Conventional commits. Scopes follow the top-level directories — `facilitator`,
  `server`, `client`, `e2e`, `examples`. A release bump is
  `chore(release): bump the client to X.Y.Z`.
- Comments say WHAT and WHY, never what changed. A reader must understand without the
  history.
- `package.json` carries its reasoning in `//`-prefixed sibling keys (`//exports`,
  `//scripts`, `//publishConfig`, …). Keep that idiom rather than dropping a bare field
  whose purpose isn't obvious.
- `gofmt` runs over `.`, not a named tree, so a new Go directory is covered without
  editing anything.

## Security invariants — never break

- **The facilitator holds no key.** It verifies offline and broadcasts a transaction the
  payer already signed, so it cannot move funds anywhere the payer did not sign for.
- **A scheme payload is relayed verbatim.** `SchemePayload` is re-emitted byte-for-byte;
  never re-encode a `transaction` field on the way through.
- **Everything decidable offline is decided before the node is touched**, so a payment
  that cannot settle costs the facilitator nothing.
- **A reason code names something established about the payment.** A facilitator's own
  outage is not that: when the chain did not answer, return `503` with no verdict. A false
  failure tells a seller to discard a response the buyer paid for.
- **The reason vocabulary is pinned by `TestReasonVocabulary`**, and the v2 header
  contract by the `TestConformance_*` tests, transcribed from the specification. Adding
  or renaming a code means updating them in the same change.
- **No npm token exists in this repository.** Publishing is OIDC trusted publishing; see
  `docs/releasing.md`.

## Housekeeping — what to update when

| When you… | Update |
|---|---|
| Add or rename a reason code | `facilitator/types.go` · `TestReasonVocabulary` |
| Change the payload or the wire types | `facilitator/types.go` and its test · the client's payload builder in `client/src/exact/client.ts` — both halves sign the same bytes |
| Rename `.github/workflows/release.yml` | the npm **trusted publisher** on npmjs.com, which matches the filename exactly. Publishing breaks until it does |
| Change the release flow or a tag scheme | `docs/releasing.md` |
| Release the client | bump `package.json`'s version — the workflow refuses a dispatch that disagrees with it. The facilitator's version is its git tag alone, so it needs no bump |
| Add a make target | this file (Commands) |

Stale docs are bugs: if a change makes a claim in a README or in `docs/` wrong, fixing it
is part of the change, not a follow-up.

## Special files

- `CLAUDE.md` is a **symlink to this file** — one brief, no second copy to drift.
