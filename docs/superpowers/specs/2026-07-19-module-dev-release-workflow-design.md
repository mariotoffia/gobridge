# Design — Dead-simple module workflow: local dev + `go get`-able releases

**Date:** 2026-07-19
**Status:** Approved (design), pending implementation plan
**Owner:** Mario Toffia
**Closes:** PROD_READY_ISSUES.md MQTT-C1/C2 (packaging blocker — `go get` fails)

## 1. Problem

GoBridge is a 52-module Go workspace (31 published, per `scripts/release/modules.json`).
Two capabilities are needed and neither has a simple, safe front door today:

1. **Local development** — a change in one internal module must be visible to sibling
   modules with no publishing step.
2. **Exported modules** — every published module must be consumable with `go get`
   (and `go install` for the binary), which forbids `replace` directives in the
   *tagged* `go.mod` and requires real path-prefixed version tags.

### What already exists (do not rebuild)

- **Local dev** already uses a Go workspace (`go.work`), the idiomatic mechanism.
  But `go.work` is gitignored (`.gitignore:44-45`) and **no `make` target creates it** —
  a fresh clone has no workspace; setup is manual and undocumented. Cross-module
  resolution currently leans on committed `replace` directives.
- **Release** already has a full, tested tool: `scripts/release/` (~170 KB Go),
  `modules.json` (canonical 4-layer DAG), and 15 make targets. `RELEASE.md` (357 lines)
  documents the policy. **It has never been run** — no path-prefixed module tags exist
  (only root `v0.1.0`/`v0.2.0`), so `go get` fails. The actual release run is a
  hand-copied bash procedure in `RELEASE.md` §4 — the "easy to do wrong" part.
- **Docs:** `AGENTS.md` points at `RELEASE.md` + `DEVELOPMENT.md`. No `MODULES.md`.

### The core tension (why this is subtle, not just missing wiring)

Committed `replace` directives are what make local dev "just work" on a fresh clone —
and they are exactly what breaks `go get`/`go install` for a *binary* module.
**Before the first release exists there are no versions to `require`, so interdependent
modules cannot build standalone (`GOWORK=off`) without either replaces or a workspace.**
The replaces are therefore a legitimate pre-first-release bootstrap; the release tool
strips them per-tag once lower-layer tags exist. So the fix is the two missing *front
doors* plus docs — **not** removing the replaces and **not** rebuilding the tool.

## 2. Goals / Non-goals

**Goals**
- One command for local dev setup that is impossible to get stale.
- One command to cut a release that is safe by default (dry-run) and dependency-ordered.
- A single `MODULES.md` that tells a human or agent how to do all three module tasks
  right every time; linked from `AGENTS.md`.
- A lint-time guard so a newly added published module can't be forgotten by the release.
- Net-new developer interface: **2 targets** (`make dev`, `make release`).

**Non-goals**
- Rebuilding or slimming the `scripts/release` tool (decision: *Keep & wrap*).
- Removing committed `replace` directives (they are the bootstrap; the tool strips per-tag).
- Committing `go.work` (decision: keep gitignored + regenerate).
- Actually executing the first public release train (ops action; needs GitHub tag
  ruleset + release principals). We build and document the button; the maintainer pushes it.
- Migrating to `multimod` (viable future simplification; see §7).

## 3. Design

### 3.1 Local development — `make dev`

Regenerate `go.work` **fresh from what is on disk** every run (never stale, no
hand-maintained module list):

```
rm -f go.work go.work.sum
go work init
go work use -r .                       # discovers every module dir recursively
go work edit -dropuse ./scripts/release # release tool must build as an external
                                        # consumer (always GOWORK=off) — never in-workspace
go work sync
```

- `go work use -r .` + the single documented `dropuse` reproduces the exact curated
  51-entry set of the current `go.work` **by discovery, not by a list**.
- Fresh clone: `make dev && make build`. Add a module: `make dev` (auto-discovered).
- `go.work` stays gitignored (unchanged convention). Standalone/external correctness
  keeps being verified by the repo's existing `GOWORK=off` usage in test/lint/release —
  that env var is the whole safety story and is already pervasive.
- `make build` ensures the workspace first: if `go.work` is absent it runs `make dev`,
  so a newcomer cannot forget.

**Acceptance:** after `make dev` on a clean checkout, the `use` set of the generated
`go.work` equals the current curated set (diff empty except the intended `scripts/release`
exclusion). Any other discovered-but-unwanted module dirs (e.g. `testdata` modules) are
added to the documented `dropuse` list during implementation.

### 3.2 Exported modules / `go get` — `make release VERSION=vX.Y.Z`

Keep the tool, `modules.json`, and the 15 low-level targets untouched. Add a thin,
guarded wrapper `scripts/release/run.sh`, invoked by a new `make release` target, that
runs the exact train from `RELEASE.md` §4 in dependency order.

**Guardrails (this is the "hard to do wrongly" surface):**
- **Preflight, fail-closed:** clean working tree; on a `release/*` branch; `VERSION`
  matches `^v[0-9]+\.[0-9]+\.[0-9]+$`; `gh` authenticated; Go present.
- **`DRY_RUN=1` is the default.** Prints the plan (modules per layer, tag names, order)
  and pushes nothing. A live run requires explicit `CONFIRM=1` (or `DRY_RUN=0`).
- **Sequence** (each step delegates to an existing tested target):
  1. `stage-published-module` root → commit → tag → push → wait proxy.
  2. `stage-release-bootstrap` + `derive-release-bootstrap` for internal helpers.
  3. For layer 1 → 2 → 3: for each module, `stage-published-module` → tag → push →
     wait for release workflow → wait for proxy. Never batch tag pushes.
  4. `smoke-released-modules` on the final `cmd/gobridge/VERSION` tag.
- **Never retag** (policy §7 of RELEASE.md): a failed step stops the run; recovery is a
  new patch train, surfaced in the wrapper's error message.
- One-time **GitHub tag-ruleset** setup stays a documented manual prerequisite (account
  config, not code).

**Result:** releasing is `make release VERSION=v0.3.0 CONFIRM=1`. `RELEASE.md` §4's
copy-paste bash is deleted in favor of the wrapper; the rest of `RELEASE.md` stays as the
deep reference.

### 3.3 Documentation — `MODULES.md` + `AGENTS.md` link

New root `MODULES.md`: the single "do it right every time" front door. Three sections:

- **(a) Local dev** — `make dev`; what the workspace is; `GOWORK=off` to test as a consumer.
- **(b) Add a module** — create `go.mod`; add the bootstrap `replace` (until first tag);
  `make dev`; if published, register in `modules.json` with the correct layer; `make lint`.
- **(c) Cut a release** — prerequisites, `make release` (dry-run then `CONFIRM=1`),
  what to do if a step fails (new patch train, never retag). Links to `RELEASE.md` for depth.

`AGENTS.md`: add one row to the task→doc table pointing module/workspace/release tasks at
`MODULES.md` (which then fans out to `RELEASE.md`/`DEVELOPMENT.md`). `DEVELOPMENT.md`'s
module section points at `MODULES.md`. No content duplication: `MODULES.md` = simple
"what/how", `RELEASE.md` = deep "why".

### 3.4 Hard-to-do-wrong guard — `modules-check` (in `make lint`)

A check that fails when the on-disk published-module set and `modules.json` disagree:
- Every `go.mod` under `adapters/**`, `processors/**`, plus `httpapi`, `cmd/gobridge`,
  and root MUST appear in `modules.json.published_modules`.
- Every `published_modules` path MUST exist on disk.

Implementation preference (ponytail — reuse first): if `make verify-release-preparation`
(`scripts/release ... source`) already inventories modules and flags an unregistered one,
run it in `make lint` and add nothing. Only if it does not, add a ~15-line shell/awk check
or a read-only `check-registration` verb to the existing tool (it already parses
`modules.json`). Decide during implementation by reading the tool's `source` command.

## 4. Make target reference (net new)

| Target | Purpose |
|---|---|
| `make dev` | Regenerate `go.work` from disk (local-dev bootstrap). Idempotent. |
| `make release VERSION=vX.Y.Z [CONFIRM=1]` | Run the dependency-ordered release train. Dry-run by default. |
| `make modules-check` | Lint-internal (run by `make lint`), not a developer front door: fail on modules.json ↔ disk drift. |

The two developer-facing front doors are `make dev` and `make release`; `modules-check`
is plumbing that `make lint` calls. Existing 15 release targets remain the tool's internal
verbs — `MODULES.md` instructs using `make release`, not them.

## 5. Files touched

- `Makefile` — add `dev`, `release`, `modules-check`; make `build` ensure the workspace.
- `scripts/release/run.sh` — new guarded wrapper (the only new script).
- `MODULES.md` — new.
- `AGENTS.md` — one new table row.
- `DEVELOPMENT.md` — repoint module section at `MODULES.md`.
- `RELEASE.md` — replace §4 hand-run bash with a pointer to `make release`; keep the rest.
- (maybe) `scripts/release/*.go` — only if adding a read-only `check-registration` verb.

No changes to any published module's `go.mod`, no changes to the tool's release logic.

## 6. Verification

- `make dev` on a clean checkout → generated `go.work` use-set == current curated set.
- `make build` from a clone with no `go.work` → succeeds (runs `dev` first).
- `make release VERSION=v9.9.9` (dry-run default) → prints correct per-layer plan, pushes
  nothing, exits 0. Preflight negatives fail closed: dirty tree, non-`release/*` branch,
  and a malformed `VERSION` (e.g. `v1.2`, `v0.3.0-rc1`) each abort before any tag.
- `make lint` → fails if a module dir is added under `adapters/` without a `modules.json`
  entry (add a temporary fixture module in a test, or assert via the tool's unit tests).
- Existing `make lint` / `make test` stay green.

## 7. Risks & future

- **Live `make release` is one-way** (immutable tags). Mitigation: dry-run default +
  explicit `CONFIRM=1` + fail-closed preflight + "never retag" guidance.
- **`go work use -r .` over-discovery** (testdata modules). Mitigation: documented
  `dropuse` list; acceptance test pins the set.
- **Future simplification (out of scope):** the research shows a ~30-module repo releasing
  at one shared version could replace the 170 KB tool with `multimod`
  (`go.opentelemetry.io/build-tools/multimod`) + a `versions.yaml` module set + ~6 targets.
  The `make release` wrapper is the seam that would let that swap happen later without
  changing the developer interface. Not done now (Keep & wrap decision).

## 8. References (research)

- Go workspaces reference & the commit-vs-gitignore guidance: <https://go.dev/ref/mod>
  (workspaces), <https://go.dev/doc/tutorial/workspaces>. Go team blesses committing
  `go.work` for self-contained monorepos but requires CI `GOWORK=off`; we keep it
  gitignored, consistent with existing convention.
- `replace` ignored for dependencies but breaks `go install pkg@version`:
  golang/go#44840.
- OpenTelemetry `multimod`/`crosslink` + `versions.yaml` (reference multi-module release
  tooling): build-tools repo READMEs.
- Kubernetes `staging/` + publishing-bot (polyrepo publishing — deliberately *not*
  applicable to same-repo path-prefixed tags).
- aws-sdk-go-v2 repotools (justified only at ~274 independently-versioned modules).
