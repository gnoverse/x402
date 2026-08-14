# Releasing

Two components, two independent version lines. Releasing one does not touch the other.

| Component | Tag | Produces |
|---|---|---|
| `facilitator` | `vX.Y.Z` | archives + GitHub release + `ghcr.io/gnoverse/gnofacilitator`, attested. Also the Go module version. |
| `client` | `client/vX.Y.Z` | `@gnoverse/x402-gno` on npm, with provenance |

Both run from the manual `release.yml` workflow. Nothing releases on push or merge.

## Release the facilitator

```sh
gh workflow run release.yml --ref main -f component=facilitator -f version=0.2.0
```

## Release the client

The published version comes from `package.json`. Bump it first, in its own PR:

```sh
npm version --no-git-tag-version 0.2.0
git commit -am "chore(release): bump the client to 0.2.0"
# merge to main, then:
gh workflow run release.yml --ref main -f component=client -f version=0.2.0
```

The workflow refuses the dispatch if `package.json` disagrees with `version`, or if that
version is already on the registry.

## Rules

- **Release from `main` only.** The workflow refuses any other ref.
- **Never delete a `vX.Y.Z` tag.** It is a Go module version; once fetched, `sum.golang.org`
  binds it to that commit permanently, and re-cutting it breaks every downstream build. Cut
  the next patch number instead. `client/vX.Y.Z` has no such constraint.
- **Use `npm version`, not a hand edit.** It updates `package-lock.json` too, which `npm ci`
  does not check.
- **Renaming `release.yml` breaks npm publishing** until npm's trusted publisher is updated
  to the new filename.

## If a release fails

- **client** — publish runs before the tag, so a failed publish leaves nothing; re-dispatch.
  A published version with no tag is fixed by `git push origin client/vX.Y.Z`.
- **facilitator** — the tag is pushed before goreleaser, so a failure can leave the tag, and
  possibly a release and image, in place. Delete the GitHub release if one exists and cut the
  next patch number.

## Credentials

npm publishing uses OIDC trusted publishing: no npm token exists in this repository. npmjs.com
matches the request against the repository and the workflow filename.

A mismatch surfaces as `E404 package not found` or `ENEEDAUTH`, neither of which reflects the
real cause — check the workflow filename and the npm version in the job first.
