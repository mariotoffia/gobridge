# Module Dev + Release Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give GoBridge two dead-simple front doors — `make dev` (local workspace) and `make release` (dependency-ordered publish) — plus a `MODULES.md` so both are done right every time, without rebuilding the existing release tooling.

**Architecture:** Keep the tested `scripts/release` tool and `modules.json` untouched. Add a `make dev` target that regenerates the gitignored `go.work` from disk; a guarded `scripts/release/run.sh` wrapper (dry-run by default) invoked by `make release` that runs the train currently hand-pasted in `RELEASE.md` §4; a `make modules-check` that reuses the tool's existing registration check (`source`) inside `make lint`; and `MODULES.md` linked from `AGENTS.md`.

**Tech Stack:** GNU Make, Bash, Go 1.25 workspaces (`go work`), the existing `scripts/release` Go CLI.

## Global Constraints

- Go 1.25+; multi-module workspace (52 `go.mod`, 31 published per `scripts/release/modules.json`).
- `go.work`/`go.work.sum` stay **gitignored** (`.gitignore:43-45`). Never commit them.
- The release tool is **kept as-is** — no changes to its release logic or `modules.json`. Reuse only.
- The workspace must include every module **except `./scripts/release`** (it always builds with `GOWORK=off` as an external consumer).
- Live releases are one-way (immutable tags): dry-run is the default; live requires `CONFIRM=1`. Never retag (RELEASE.md policy §7).
- Doc file length ≤ 600 lines; code file length ≤ 500 lines (repo rule).
- No new external tool dependency (jq etc. must not be required by any target).

---

### Task 1: `make dev` — regenerate the workspace from disk

**Files:**
- Modify: `Makefile` — add `dev` target; add `dev` to `.PHONY`; guard `build`.

**Interfaces:**
- Produces: `make dev` (regenerates `go.work`), and `make build` auto-runs `dev` when `go.work` is absent.

- [ ] **Step 1: Write the failing test (workspace-completeness check)**

Run this from the repo root — it asserts the workspace `use` set equals every on-disk module dir except `scripts/release`:

```bash
diff \
  <(awk '/^use \(/{f=1;next} /^\)/{f=0} f{gsub(/^[ \t]+/,"");print}' go.work | sort) \
  <(find . -name go.mod -not -path './scripts/release/*' -not -path './vendor/*' -not -path './.git/*' -exec dirname {} \; | sort)
```

- [ ] **Step 2: Establish the baseline**

Run the Step 1 command against the *current* (already-present) `go.work`.
Expected: **empty output, exit 0** — the current curated workspace already equals disk-minus-`scripts/release`. This is the invariant `make dev` must reproduce from scratch. (If non-empty, record the difference; the exclude list in Step 3 must be widened to match.)

- [ ] **Step 3: Add the `dev` target**

Add the target immediately after the `build` target (after line 55), with its own
`.PHONY` line (Make allows multiple `.PHONY` declarations — this keeps each task's edit
independent and conflict-free):

```make
.PHONY: dev
dev: ## Regenerate the Go workspace (go.work) from every on-disk module (local-dev bootstrap)
	@echo "Regenerating go.work from on-disk modules..."
	@rm -f go.work go.work.sum
	@go work init
	@go work use -r .
	@go work edit -dropuse ./scripts/release
	@go work sync
	@echo "Workspace ready. (scripts/release is excluded by design — it builds with GOWORK=off.)"
```

- [ ] **Step 4: Guard `build` so newcomers can't forget**

Modify the `build` target (lines 53-55) to ensure the workspace exists first:

```make
build: ## Build all modules
	@test -f go.work || $(MAKE) dev
	@echo "Building all modules..."
	go build ./...
```

- [ ] **Step 5: Run `make dev`, then the check, and verify it passes**

```bash
make dev
diff \
  <(awk '/^use \(/{f=1;next} /^\)/{f=0} f{gsub(/^[ \t]+/,"");print}' go.work | sort) \
  <(find . -name go.mod -not -path './scripts/release/*' -not -path './vendor/*' -not -path './.git/*' -exec dirname {} \; | sort)
```
Expected: **empty output, exit 0**. Then confirm a clean-clone simulation:
```bash
rm -f go.work go.work.sum && make build
```
Expected: `make` runs `dev` first, then `go build ./...` succeeds.

- [ ] **Step 6: Commit**

```bash
git add Makefile
git commit -m "feat(make): add 'make dev' to regenerate go.work from disk; build ensures workspace"
```

---

### Task 2: `make modules-check` — fail on modules.json ↔ disk drift (reuse), wired into lint

**Files:**
- Modify: `Makefile` — add `modules-check` target + `.PHONY`; call it from `lint`.

**Interfaces:**
- Consumes: the existing `scripts/release` `source` command (`runSourcePreflight` → `inspectRepository` → `validatePublishedSet`), which already errors `"canonical published set differs from repository policy: missing=… extra=…"` when an on-disk published module is unregistered (or vice versa).
- Produces: `make modules-check`; `make lint` fails when the published set drifts.

- [ ] **Step 1: Write the failing test (unregistered module is rejected)**

Prove the guard catches a new, unregistered published module. Create a throwaway module, then run the check:

```bash
mkdir -p adapters/ztest/transport/zz
printf 'module github.com/mariotoffia/gobridge/adapters/ztest/transport/zz\n\ngo 1.25.0\n' > adapters/ztest/transport/zz/go.mod
cd scripts/release && GOWORK=off go run . source --repo ../.. ; echo "exit=$?" ; cd ../..
```
Expected: **non-zero exit**, message contains `missing=[adapters/ztest/transport/zz]`.

- [ ] **Step 2: Confirm the clean tree passes**

```bash
rm -rf adapters/ztest
cd scripts/release && GOWORK=off go run . source --repo ../.. ; echo "exit=$?" ; cd ../..
```
Expected: **exit 0**, prints `Release source preflight PASS.` and the module inventory.

- [ ] **Step 3: Add the `modules-check` target**

Add the target near the other release targets (e.g. directly after the
`verify-release-preparation` target, around line 82), with its own `.PHONY` line:

```make
.PHONY: modules-check
modules-check: ## Fail if the on-disk published-module set drifts from scripts/release/modules.json
	@mkdir -p reports
	@echo "=== published-module registration ==="
	@bash -c 'set -o pipefail; cd scripts/release && GOWORK=off go run . source --repo ../.. 2>&1 | tee $(PWD)/reports/modules-check.log'
```

- [ ] **Step 4: Wire it into `lint`**

Add `modules-check` as the first recipe step of `lint`. Change the top of the `lint` recipe (after line 269 `lint:` and line 270 `@mkdir -p reports`) to insert the call:

```make
lint: build-aclcheck build-aggcheck build-cfgshape build-registrychk build-pluginsym ## Run every static check (arch, gofmt, go vet, golangci-lint, aggcheck, aclcheck, cfgshape, registrychk, pluginsym); writes reports/*
	@mkdir -p reports
	@$(MAKE) --no-print-directory modules-check
	@echo "=== Architecture lint ==="
```

- [ ] **Step 5: Run and verify**

```bash
make modules-check          # exit 0, PASS
make lint 2>&1 | tail -5    # completes as before, plus a modules-check stage
```
Expected: `modules-check` PASS; `make lint` still green (pre-existing failures unrelated to this change are out of scope but must not be newly introduced).

- [ ] **Step 6: Commit**

```bash
git add Makefile
git commit -m "feat(make): add 'make modules-check' (reuses release source gate) and run it in lint"
```

---

### Task 3: `make release` — one-command, dry-run-default release train

**Files:**
- Create: `scripts/release/run.sh`
- Modify: `Makefile` — add `release` target + vars + `.PHONY`.

**Interfaces:**
- Consumes: existing targets `release-modules`, `stage-published-module`, `stage-release-bootstrap`, `derive-release-bootstrap`, `smoke-released-modules` (all documented in `RELEASE.md`).
- Produces: `make release VERSION=vX.Y.Z [CONFIRM=1] [DRY_RUN=0]`. Env read by `run.sh`: `VERSION`, `CONFIRM` (default `0`), `DRY_RUN` (default `1`, forced by `CONFIRM`), `REMOTE` (default `origin`).

- [ ] **Step 1: Write the failing test (dry-run prints a plan and pushes nothing)**

```bash
before=$(git tag | wc -l)
VERSION=v9.9.9 bash scripts/release/run.sh
after=$(git tag | wc -l)
[ "$before" = "$after" ] && echo "OK no tags created" || echo "FAIL tags changed"
```
Expected before the file exists: **fails** (`run.sh: No such file or directory`).

- [ ] **Step 2: Create `scripts/release/run.sh`**

```bash
#!/usr/bin/env bash
# scripts/release/run.sh — one-command, dependency-ordered release train.
# Mechanizes RELEASE.md §1–§5. Dry-run by default; CONFIRM=1 pushes immutable tags.
#
# Env: VERSION=vX.Y.Z (required)  CONFIRM=1 (publish)  DRY_RUN=1 (default; forced 1 unless CONFIRM=1)
#      REMOTE=origin
set -euo pipefail

VERSION="${VERSION:-}"
CONFIRM="${CONFIRM:-0}"
REMOTE="${REMOTE:-origin}"
if [ "$CONFIRM" = "1" ]; then DRY_RUN="${DRY_RUN:-0}"; else DRY_RUN=1; fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

die() { echo "release: $*" >&2; exit 1; }

# ---- preflight (always) ----
[ -n "$VERSION" ] || die "VERSION is required (e.g. VERSION=v0.3.0)"
printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' \
  || die "VERSION must match vX.Y.Z with no prerelease/build metadata (got: $VERSION)"
command -v go >/dev/null 2>&1 || die "go not found on PATH"

# ---- preflight (live only) ----
if [ "$DRY_RUN" != "1" ]; then
  git diff --quiet && git diff --cached --quiet || die "working tree is not clean; commit or stash first"
  branch="$(git rev-parse --abbrev-ref HEAD)"
  case "$branch" in
    release/*) : ;;
    *) die "live release must run on a release/* branch (current: $branch)";;
  esac
  command -v gh >/dev/null 2>&1 || die "gh CLI not found (needed to wait on release workflows)"
  gh auth status >/dev/null 2>&1 || die "gh is not authenticated (run: gh auth login)"
fi

# ---- plan (always) ----
echo "Release plan for ${VERSION} (DRY_RUN=${DRY_RUN}, REMOTE=${REMOTE}):"
for layer in 0 1 2 3; do
  mods="$(make --no-print-directory release-modules RELEASE_LAYER="$layer" 2>/dev/null || true)"
  [ -n "$mods" ] || continue
  echo "  layer ${layer}:"
  while IFS= read -r m; do
    [ -n "$m" ] || continue
    if [ "$m" = "." ]; then echo "    (root) -> ${VERSION}"; else echo "    ${m} -> ${m}/${VERSION}"; fi
  done <<EOF
$mods
EOF
done

if [ "$DRY_RUN" = "1" ]; then
  echo "Dry run only — nothing staged, committed, tagged, or pushed."
  echo "Re-run with CONFIRM=1 on a release/* branch to publish."
  exit 0
fi

# =====================================================================
# LIVE PUBLISH — mechanizes RELEASE.md §1–§5. One-way; never retag.
# =====================================================================
wait_for_proxy() {
  local module="$1"
  until GOWORK=off GOPROXY=https://proxy.golang.org go list -m "${module}@${VERSION}"; do
    echo "waiting for proxy.golang.org: ${module}@${VERSION}" >&2
    sleep 15
  done
}
wait_for_release_workflow() {
  local tag="$1" run_id="" _
  for _ in $(seq 1 30); do
    run_id="$(gh run list --workflow release.yml --event push --branch "$tag" \
      --limit 1 --json databaseId --jq '.[0].databaseId // empty')"
    if [ -n "$run_id" ]; then gh run watch "$run_id" --exit-status; return; fi
    sleep 5
  done
  die "release workflow did not appear for $tag"
}
import_for() { # module dir -> import path
  if [ "$1" = "." ]; then echo "github.com/mariotoffia/gobridge"; else echo "github.com/mariotoffia/gobridge/$1"; fi
}
tag_for() { # module dir -> tag
  if [ "$1" = "." ]; then echo "$VERSION"; else echo "$1/$VERSION"; fi
}

# §2 root
echo "== §2 root =="
make stage-published-module RELEASE_MODULE=. RELEASE_VERSION="$VERSION"
if ! git diff --quiet -- go.mod go.sum; then
  git add go.mod go.sum && git commit -m "release: root ${VERSION}"
fi
git tag "$VERSION"
git push "$REMOTE" "$VERSION"
wait_for_release_workflow "$VERSION"
wait_for_proxy "$(import_for .)"

# §3 bootstrap internal test helpers
echo "== §3 bootstrap =="
make stage-release-bootstrap RELEASE_VERSION="$VERSION"
git add testutil/*/go.mod
git commit -m "release: bootstrap test helpers for ${VERSION}"
git push "$REMOTE" "HEAD:refs/heads/${branch}"
BOOTSTRAP_COMMIT="$(git rev-parse HEAD)"
make derive-release-bootstrap RELEASE_VERSION="$VERSION" RELEASE_BOOTSTRAP_COMMIT="$BOOTSTRAP_COMMIT"

# §4 layers 1..3
for layer in 1 2 3; do
  echo "== §4 layer ${layer} =="
  while IFS= read -r module; do
    [ -n "$module" ] || continue
    make stage-published-module RELEASE_MODULE="$module" RELEASE_VERSION="$VERSION" \
      RELEASE_BOOTSTRAP_COMMIT="$BOOTSTRAP_COMMIT"
    git add "${module}/go.mod"
    [ -f "${module}/go.sum" ] && git add "${module}/go.sum" || true
    git commit -m "release: ${module} ${VERSION}"
    tag="$(tag_for "$module")"
    git tag "$tag"
    git push "$REMOTE" "$tag"
    wait_for_release_workflow "$tag"
    wait_for_proxy "$(import_for "$module")"
  done < <(make --no-print-directory release-modules RELEASE_LAYER="$layer")
done

# §5 final public proof
echo "== §5 smoke =="
make smoke-released-modules RELEASE_TAG="cmd/gobridge/${VERSION}"
echo "Release ${VERSION} complete."
```

Make it executable:
```bash
chmod +x scripts/release/run.sh
```

- [ ] **Step 3: Add the `release` target and vars to `Makefile`**

Add vars after the `RELEASE_*` block (after line 44):
```make
VERSION ?=
CONFIRM ?= 0
DRY_RUN ?=
```

Add the target after `modules-check`, with its own `.PHONY` line:
```make
.PHONY: release
release: ## Run the dependency-ordered release train. Dry-run by default; CONFIRM=1 publishes. Requires VERSION=vX.Y.Z
	@VERSION="$(VERSION)" CONFIRM="$(CONFIRM)" DRY_RUN="$(DRY_RUN)" REMOTE="$(RELEASE_REMOTE)" bash scripts/release/run.sh
```

- [ ] **Step 4: Run the dry-run tests and verify they pass**

```bash
before=$(git tag | wc -l)
make release VERSION=v9.9.9 | tee /dev/stderr | grep -q "Dry run only" && echo "OK dry-run"
[ "$before" = "$(git tag | wc -l)" ] && echo "OK no tags created"
# negative cases fail closed:
make release VERSION=v1.2      ; echo "exit=$? (want non-zero: bad version)"
make release VERSION=v0.3.0-rc1; echo "exit=$? (want non-zero: prerelease)"
make release                   ; echo "exit=$? (want non-zero: missing VERSION)"
```
Expected: dry-run prints the per-layer plan + "Dry run only …", creates no tags, exit 0; each negative case exits non-zero before any git action.

- [ ] **Step 5: Commit**

```bash
git add scripts/release/run.sh Makefile
git commit -m "feat(release): add 'make release' one-command train (dry-run default, CONFIRM=1 to publish)"
```

---

### Task 4: `MODULES.md` — the single "do it right every time" doc

**Files:**
- Create: `MODULES.md`

**Interfaces:**
- Consumes: `make dev`, `make release`, `make modules-check`, and `scripts/release/modules.json` from Tasks 1-3.

- [ ] **Step 1: Write the failing test (doc exists and covers the three tasks)**

```bash
test -f MODULES.md \
  && grep -q 'make dev' MODULES.md \
  && grep -q 'make release' MODULES.md \
  && grep -q 'modules.json' MODULES.md \
  && echo "OK" || echo "FAIL"
```
Expected before creation: `FAIL`.

- [ ] **Step 2: Create `MODULES.md`**

```markdown
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
- To test a module exactly as an external consumer receives it (no workspace), use
  `GOWORK=off` — this is how `make test`, `make lint`, and the release gates already run.

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
```

- [ ] **Step 3: Run the test and verify it passes**

```bash
test -f MODULES.md && grep -q 'make dev' MODULES.md && grep -q 'make release' MODULES.md && grep -q 'modules.json' MODULES.md && echo "OK"
wc -l MODULES.md   # must be < 600
```
Expected: `OK`; line count well under 600.

- [ ] **Step 4: Commit**

```bash
git add MODULES.md
git commit -m "docs: add MODULES.md — local dev, adding a module, cutting a release"
```

---

### Task 5: Wire the docs together (AGENTS.md, DEVELOPMENT.md, RELEASE.md)

**Files:**
- Modify: `AGENTS.md` — add a task→doc row for modules/workspace/release.
- Modify: `DEVELOPMENT.md` — point the module section at `MODULES.md`.
- Modify: `RELEASE.md` — replace the §4 hand-run bash with a pointer to `make release`; keep the rest as the deep reference.

**Interfaces:**
- Consumes: `MODULES.md` from Task 4.

- [ ] **Step 1: Write the failing test (AGENTS.md points at MODULES.md)**

```bash
grep -q 'MODULES.md' AGENTS.md && echo "OK" || echo "FAIL"
```
Expected before edit: `FAIL`.

- [ ] **Step 2: Add the AGENTS.md row**

In `AGENTS.md`, in the "MUST: Where to look — task → doc" table, add this row directly
above the existing "Tagging a release, bumping inter-module requires…" row (line 22):

```markdown
| Local dev workspace setup, adding a module, or cutting a release (the simple front door) | `MODULES.md` (`make dev` / `make release`) |
```

- [ ] **Step 3: Point DEVELOPMENT.md at MODULES.md**

In `DEVELOPMENT.md`, under the "Repository Structure" section intro (the paragraph that
begins "gobridge is a multi-module Go workspace."), append one sentence:

```markdown
For the everyday workflow — bootstrapping the workspace, adding a module, and cutting a
`go get`-able release — see [MODULES.md](MODULES.md); it is the simple front door and
links here and to [RELEASE.md](RELEASE.md) for depth.
```

- [ ] **Step 4: Replace RELEASE.md §4's hand-run bash with the wrapper pointer**

In `RELEASE.md`, replace the body of "### 4. Stage, tag, and push each dependency layer"
(the `release_module()` shell function and its `for layer in 1 2 3` loop) with:

```markdown
This dependency-ordered stage/tag/push/wait loop is mechanized by
[`scripts/release/run.sh`](scripts/release/run.sh), invoked as `make release
VERSION=vX.Y.Z CONFIRM=1`. See [MODULES.md §3](MODULES.md#3-cut-a-release-make-it-go-get-able).
Run `make release VERSION=vX.Y.Z` first (dry-run) to review the per-layer plan. The
sections below (§1 waits, §2 root, §3 bootstrap, §5 smoke) document what each step does;
`run.sh` performs them in order and must not be bypassed to retag.
```

Leave §1, §2, §3, §5 as the authoritative description of each step (run.sh implements them).

- [ ] **Step 5: Run the tests and verify**

```bash
grep -q 'MODULES.md' AGENTS.md && grep -q 'MODULES.md' DEVELOPMENT.md && grep -q 'run.sh' RELEASE.md && echo "OK"
for f in AGENTS.md DEVELOPMENT.md RELEASE.md MODULES.md; do echo "$f: $(wc -l < $f) lines"; done
```
Expected: `OK`; every doc < 600 lines.

- [ ] **Step 6: Commit**

```bash
git add AGENTS.md DEVELOPMENT.md RELEASE.md
git commit -m "docs: link MODULES.md from AGENTS/DEVELOPMENT; point RELEASE.md §4 at make release"
```

---

## Final verification (run after all tasks)

- [ ] `make dev` → workspace check (Task 1 Step 5) empty diff.
- [ ] `rm -f go.work go.work.sum && make build` → succeeds (regenerates workspace).
- [ ] `make modules-check` → PASS; `make lint` green (no newly introduced failures).
- [ ] `make release VERSION=v9.9.9` → prints plan, creates no tags, exit 0; bad versions fail closed.
- [ ] `make test` → green (unchanged).
- [ ] All four docs (`MODULES.md`, `AGENTS.md`, `DEVELOPMENT.md`, `RELEASE.md`) < 600 lines and cross-linked.

## Self-review notes (spec coverage)

- Spec §3.1 (local dev) → Task 1. §3.2 (`make release`) → Task 3. §3.3 (docs) → Tasks 4-5.
  §3.4 (guard) → Task 2 (confirmed pure reuse: `validatePublishedSet` already hard-errors).
- Spec §5 files-touched all covered; no changes to published `go.mod` or tool release logic.
- No placeholders: every code/edit step shows exact content and an expected result.
- Not implemented by design (spec §2 non-goals): removing `replace` bootstrap, committing
  `go.work`, executing the first real train, `multimod` migration.
