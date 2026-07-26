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

# §4 layers 1..3
for layer in 1 2 3; do
  echo "== §4 layer ${layer} =="
  while IFS= read -r module; do
    [ -n "$module" ] || continue
    publish_module "$module"
  done < <(make --no-print-directory release-modules RELEASE_LAYER="$layer")
done

# §5 final public proof
echo "== §5 smoke =="
make smoke-released-modules RELEASE_TAG="cmd/gobridge/${VERSION}"
echo "Release ${VERSION} complete."
