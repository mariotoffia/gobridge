# Modules: local development & releasing

GoBridge is a multi-module Go workspace: each adapter, processor, `httpapi`, and
`cmd/gobridge` is its own module so consumers `go get` only what they need. This is
the single front door for the three module tasks. Deep release policy lives in
[RELEASE.md](RELEASE.md); this file is the "how", that file is the "why".

## 1. Local development

The workspace file `go.work` ties every module together for local builds. It is
**gitignored and generated** — never commit it.

```bash
make dev     # regenerate go.work from every on-disk module (except scripts/release)
make build   # runs `make dev` automatically if go.work is missing
```

- Fresh clone: `make dev && make build`.
- `make dev` discovers modules from disk, so it is never stale — re-run it any time.
- Plain `make test`/`make lint` run **inside** the workspace, so they can't catch a module that uses an unreleased sibling API without bumping its `require` ("the workspace can lie" — see [DEVELOPMENT.md](DEVELOPMENT.md)). To check a module as an external consumer sees it, run `GOWORK=off go build ./...` in that module's directory; the release process re-verifies every published module with the workspace disabled before tagging (see [RELEASE.md](RELEASE.md)).

## 2. Add a new module

1. Create the module directory with a `go.mod`
   (`github.com/mariotoffia/gobridge/<path>`), following [PLUGIN.md](PLUGIN.md) for
   adapters/processors.
2. Until the first release tag of a sibling it depends on exists, add the bootstrap
   `replace` directives so it builds standalone (see an existing sibling's `go.mod`).
   The release tool strips these per-tag at publish time — do not remove them by hand.
3. `make dev` — the module joins the workspace automatically.
4. **If it is published** (anything under `adapters/`, `processors/`, or `httpapi`,
   `cmd/gobridge`, root): add it to
   [`scripts/release/modules.json`](scripts/release/modules.json) with its dependency
   `layer` (a module may only require lower layers). `make lint` runs `make
   modules-check` and **fails** if you forget this step.
5. `make lint && make test`.

## 3. Cut a release (make it `go get`-able)

Prerequisites (one-time): a GitHub tag ruleset as described in
[RELEASE.md](RELEASE.md#required-github-tag-ruleset), and `gh` authenticated.

```bash
# 1. Always dry-run first — prints the per-layer module→tag plan, pushes nothing:
make release VERSION=v0.3.0

# 2. Publish (one-way; immutable tags) from a release/* branch, clean tree:
git switch -c release/v0.3.0
make release VERSION=v0.3.0 CONFIRM=1
```

`make release` mechanizes the whole train in dependency order (root → bootstrap
helpers → layer 1 → 2 → 3 → external-consumer smoke), waiting for each tag's workflow
and proxy propagation. It is safe by default: **dry-run unless `CONFIRM=1`**, and it
refuses to run on a dirty tree, a non-`release/*` branch, or a malformed version.

**If a step fails:** stop. Do **not** delete or move a tag (policy in
[RELEASE.md](RELEASE.md#policy)). Diagnose, then start a new patch train
(e.g. `v0.3.1`).

After a successful train, consumers can:

```bash
go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho@v0.3.0
go install github.com/mariotoffia/gobridge/cmd/gobridge@v0.3.0
```
