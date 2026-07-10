# Releasing GoBridge

How to version, tag, and publish the multi-module workspace so external consumers can `go get` / `go install` every published module. Read this before creating any tag. Development-side rules (workspace, replaces, requires) live in [DEVELOPMENT.md — Module versioning & references](DEVELOPMENT.md#module-versioning--references).

> **Current state (2026-07-09):** this policy is not yet applied. Published go.mod files still carry `replace` directives and `v0.0.0` sibling requires, and only root tags `v0.1.0`/`v0.2.0` exist — external `go get` fails. The first release under this policy must complete the migration in [First release checklist](#first-release-checklist).

## Policy

1. **Single version train.** Every published module releases the same `vX.Y.Z` on the same commit train. No per-module version drift.
2. **One tag per module path.** A nested module is only fetchable via a path-prefixed tag: root → `vX.Y.Z`, submodule → `<module-dir>/vX.Y.Z` (e.g. `adapters/aws/store/v0.3.0`, `httpapi/v0.3.0`, `cmd/gobridge/v0.3.0`).
3. **No `replace` directives in published modules.** `go get` ignores them and `go install pkg@version` refuses modules that contain them. Local resolution is `go.work`'s job.
4. **Inter-module `require`s always name a resolvable published version** — during development that is the *previous* release. `go.work` overrides to HEAD locally; the proxy-facing go.mod must never point at an unpublished version.
5. **Internal-only modules** (`tests/`, `testutil/`, `scripts/`, `deployment/`) are not published: they are never tagged, and they may keep `replace` directives. Consequence: they are not `go get`-able — that is intended.

## Published module set

Root (`github.com/mariotoffia/gobridge`), `httpapi`, `cmd/gobridge`, and every module under `adapters/` and `processors/`. Everything else is internal-only.

## Release procedure (dependency-ordered)

Tag in dependency order so every module's requires resolve at the moment it is tidied and tagged. This avoids the tag/tidy chicken-and-egg and keeps `go install cmd/gobridge@vX.Y.Z` working (complete go.sum at every tag).

Order: **root → leaf store/processor modules → aggregate adapters (`adapters/native/store`, `adapters/aws/store`) → transports/config/credentials/metrics adapters → `httpapi` → `cmd/gobridge`.**

For each layer, in order:

```bash
# 1. Bump sibling requires in this layer's go.mod files to the version just tagged below it
go mod edit -require=github.com/mariotoffia/gobridge@v0.3.0 <module>/go.mod  # per sibling require

# 2. Tidy — resolves against the proxy; works because the lower layers are already tagged and pushed
(cd <module> && GOWORK=off go mod tidy)

# 3. Commit, tag, push the tag before starting the next layer
git commit -am "release: <module> v0.3.0"
git tag <module-dir>/v0.3.0
git push origin <module-dir>/v0.3.0
```

Root goes first with a plain `git tag v0.3.0`. Script the loop; do not hand-run 30 modules.

Notes:

- The proxy caches; give `proxy.golang.org` a moment (or `GOPROXY=direct` in the release script) between layers.
- Never retag or delete a published tag — module hashes are immutable in the checksum database. A broken release is fixed by releasing `vX.Y.Z+1`.
- Update `docs/release-notes.md` in the same train.

## Consumability gate (CI)

The workspace hides missing require-bumps: code can use a new sibling API at HEAD while go.mod still names the old version — it compiles locally and breaks only for consumers. Guard:

```bash
# per published module — must pass with the workspace disabled
GOWORK=off go build ./...
```

Run it in CI on every PR (resolves the *declared* requires from the proxy) and as a post-release verification of the tagged commits.

## First release checklist

Migration from the current state, once, before the next tag:

1. Remove all `replace` directives from published modules' go.mod files (keep `tests/`, `testutil/`, `scripts/`, `deployment/`).
2. Set every inter-module require to the last resolvable release once it exists — bootstrap by releasing bottom-up per the procedure above (root first, `cmd/gobridge` last).
3. Add the `GOWORK=off` consumability gate to CI.
4. Verify from a clean machine outside the repo: `go install github.com/mariotoffia/gobridge/cmd/gobridge@v0.3.0` and `go get github.com/mariotoffia/gobridge/adapters/aws/transport/sqs@v0.3.0`.
