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

# Polling pacing. 20s across a whole layer keeps API usage far below the
# 5000/hour limit even for the 26-module layer.
WORKFLOW_POLL_SECONDS="${WORKFLOW_POLL_SECONDS:-20}"
WORKFLOW_APPEAR_GRACE="${WORKFLOW_APPEAR_GRACE:-180}"
WORKFLOW_BUDGET="${WORKFLOW_BUDGET:-5400}"

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
  local tag="$1" run_id="" conclusion="" _
  for _ in $(seq 1 30); do
    run_id="$(gh run list --workflow release.yml --event push --branch "$tag" \
      --limit 1 --json databaseId --jq '.[0].databaseId // empty')"
    [ -n "$run_id" ] && break
    sleep 5
  done
  [ -n "$run_id" ] || die "release workflow did not appear for $tag"

  if gh run watch "$run_id" --exit-status >/dev/null 2>&1; then
    echo "-- ${tag}: release workflow green"
    return
  fi

  # One bounded retry. A tag cannot be re-pushed, so a run that failed on
  # something transient (proxy propagation, a registry hiccup) would otherwise
  # force an entire new version train for a fault that has already cleared.
  # The verifier retries propagation internally now, so a genuine defect fails
  # again here rather than being retried away.
  conclusion="$(gh run view "$run_id" --json conclusion --jq '.conclusion // empty')"
  echo "-- ${tag}: release workflow ${conclusion}; re-running once" >&2
  gh run rerun "$run_id" --failed >/dev/null 2>&1 || die "could not re-run workflow for $tag"
  sleep 10
  gh run watch "$run_id" --exit-status >/dev/null 2>&1 \
    || die "release workflow failed for $tag after one re-run (run $run_id)"
  echo "-- ${tag}: release workflow green after re-run"
}
# Wait for every tag in a layer using ONE API call per poll cycle.
#
# The obvious implementation — a `gh run watch` per tag, backgrounded — spawns
# one independent poller per module, each hitting the API every few seconds.
# For a 26-module layer that exhausted the 5000/hour limit mid-train and failed
# the layer on HTTP 403, even though the workflows themselves were healthy. The
# observer, not the work, was the problem.
#
# `gh run list` returns every run's state in a single response, so the polling
# cost is constant regardless of how many modules a layer holds.
#
# A tag with no run at all is treated as fatal once the grace period passes:
# that is the signature of a bulk tag push silently not triggering workflows,
# and it must never be mistaken for "still starting".
wait_for_layer_workflows() {
  local tags=("$@")
  local start now snapshot tag state pending missing
  start="$(date +%s)"
  while :; do
    snapshot="$(gh run list --workflow release.yml --limit 100 \
      --json headBranch,status,conclusion \
      --jq '.[] | "\(.headBranch)\t\(.status)\t\(.conclusion // "-")"' 2>/dev/null)" || {
      sleep "$WORKFLOW_POLL_SECONDS"; continue
    }
    pending=0
    missing=0
    for tag in "${tags[@]}"; do
      state="$(printf '%s\n' "$snapshot" | awk -F'\t' -v t="$tag" '$1==t {print $2"/"$3; exit}')"
      case "$state" in
        completed/success) ;;
        "")               missing=$((missing + 1)); pending=$((pending + 1)) ;;
        completed/*)      die "release workflow ${state#completed/} for $tag" ;;
        *)                pending=$((pending + 1)) ;;
      esac
    done
    [ "$pending" -eq 0 ] && { echo "-- all ${#tags[@]} workflow(s) green"; return 0; }
    now="$(date +%s)"
    if [ "$missing" -gt 0 ] && [ $((now - start)) -gt "$WORKFLOW_APPEAR_GRACE" ]; then
      die "$missing tag(s) never triggered a release workflow (bulk push?)"
    fi
    [ $((now - start)) -lt "$WORKFLOW_BUDGET" ] || die "timed out waiting for layer workflows"
    echo "-- ${pending}/${#tags[@]} workflow(s) still running"
    sleep "$WORKFLOW_POLL_SECONDS"
  done
}

import_for() { # module dir -> import path
  if [ "$1" = "." ]; then echo "github.com/mariotoffia/gobridge"; else echo "github.com/mariotoffia/gobridge/$1"; fi
}
tag_published() { # tag -> 0 if it already exists on the remote
  git ls-remote --exit-code --tags "$REMOTE" "refs/tags/$1" >/dev/null 2>&1
}
# Publish one module unless its tag already exists.
#
# Tags are immutable, so a train that stopped part-way can only be continued,
# never restarted: re-running `git tag` on a published module aborts the whole
# thing. Skipping an already-published tag makes this script resumable, while
# still waiting for that module's workflow and proxy propagation — so a resumed
# run applies exactly the same gates as an uninterrupted one.
publish_module() { # module dir
  local module="$1" tag import
  tag="$(tag_for "$module")"
  import="$(import_for "$module")"
  if tag_published "$tag"; then
    echo "-- ${tag} already published; verifying and continuing"
  else
    make stage-published-module RELEASE_MODULE="$module" RELEASE_VERSION="$VERSION" \
      ${BOOTSTRAP_COMMIT:+RELEASE_BOOTSTRAP_COMMIT="$BOOTSTRAP_COMMIT"}
    if [ "$module" = "." ]; then
      git add go.mod go.sum 2>/dev/null || true
    else
      git add "${module}/go.mod"
      [ -f "${module}/go.sum" ] && git add "${module}/go.sum"
    fi
    git diff --cached --quiet || git commit -m "release: ${module} ${VERSION}"
    git tag "$tag"
    git push "$REMOTE" "$tag"
  fi
  wait_for_release_workflow "$tag"
  wait_for_proxy "$import"
}
tag_for() { # module dir -> tag
  if [ "$1" = "." ]; then echo "$VERSION"; else echo "$1/$VERSION"; fi
}

# Publish every module in one layer, concurrently.
#
# A layer means "these modules do not depend on each other" — that is the whole
# reason the DAG has layers. Publishing them one at a time made each tag wait a
# full workflow round-trip (~150s) before the next was even pushed, so 26
# independent layer-1 modules cost over an hour of pure queueing. Layers must
# still be sequential: staging a layer-2 module runs `go mod tidy`, which has to
# resolve its layer-1 siblings at this version from the public proxy.
#
# Staging stays sequential because it edits the working tree and commits; only
# the waiting is parallel, which is the part that actually took the time. Every
# gate is unchanged — each tag gets its own workflow, its own strict
# verification and its own proxy check.
publish_layer() { # layer number
  local layer="$1" module tag import modules=() tags=() imports=()
  while IFS= read -r module; do
    [ -n "$module" ] || continue
    modules+=("$module")
  done < <(make --no-print-directory release-modules RELEASE_LAYER="$layer")
  [ ${#modules[@]} -gt 0 ] || return 0

  # 1. stage + tag + push every module in the layer (sequential; local work)
  for module in "${modules[@]}"; do
    tag="$(tag_for "$module")"
    if tag_published "$tag"; then
      echo "-- ${tag} already published; will verify"
    else
      make stage-published-module RELEASE_MODULE="$module" RELEASE_VERSION="$VERSION" \
        ${BOOTSTRAP_COMMIT:+RELEASE_BOOTSTRAP_COMMIT="$BOOTSTRAP_COMMIT"}
      if [ "$module" = "." ]; then
        git add go.mod go.sum 2>/dev/null || true
      else
        git add "${module}/go.mod"
        [ -f "${module}/go.sum" ] && git add "${module}/go.sum"
      fi
      git diff --cached --quiet || git commit -m "release: ${module} ${VERSION}"
      git tag "$tag"
    fi
    tags+=("$tag")
    imports+=("$(import_for "$module")")
  done

  # 2. push the layer's tags ONE AT A TIME.
  #
  #    Do not batch these into a single `git push`. GitHub does not create a
  #    workflow event for every ref in a bulk tag push — past roughly three
  #    tags the remainder silently get no workflow run at all. A 26-tag atomic
  #    push therefore publishes every tag while triggering almost no
  #    verification, which is worse than slow: the tags are immutable, so the
  #    version cannot be re-verified afterwards.
  #
  #    This is not the slow part. A push is a second or two; the ~150s workflow
  #    wait is what costs time, and that is what runs concurrently below.
  local pushed=0
  for tag in "${tags[@]}"; do
    if ! tag_published "$tag"; then
      git push "$REMOTE" "refs/tags/${tag}"
      pushed=$((pushed + 1))
    fi
  done
  [ "$pushed" -eq 0 ] || echo "-- pushed ${pushed} tag(s) for layer ${layer}"

  # 3. wait for the layer's workflows with a single shared poller
  echo "-- waiting for ${#tags[@]} workflow(s) in layer ${layer}"
  wait_for_layer_workflows "${tags[@]}"

  # 4. wait for proxy propagation of the whole layer before the next one stages
  for import in "${imports[@]}"; do
    wait_for_proxy "$import"
  done
}

# §2 root
echo "== §2 root =="
publish_module .

# §3 bootstrap internal test helpers
echo "== §3 bootstrap =="
make stage-release-bootstrap RELEASE_VERSION="$VERSION"
git add testutil/*/go.mod
git diff --cached --quiet || git commit -m "release: bootstrap test helpers for ${VERSION}"
git push "$REMOTE" "HEAD:refs/heads/${branch}"
BOOTSTRAP_COMMIT="$(git rev-parse HEAD)"
make derive-release-bootstrap RELEASE_VERSION="$VERSION" RELEASE_BOOTSTRAP_COMMIT="$BOOTSTRAP_COMMIT"

# §4 layers 1..3 — sequential between layers, concurrent within each
for layer in 1 2 3; do
  echo "== §4 layer ${layer} =="
  publish_layer "$layer"
done

# §5 final public proof
echo "== §5 smoke =="
make smoke-released-modules RELEASE_TAG="cmd/gobridge/${VERSION}"
echo "Release ${VERSION} complete."
