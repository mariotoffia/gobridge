#!/usr/bin/env bash
# update-image.sh — pin image.txt and the seeder Dockerfile FROM to the current
# top-level multi-platform digest of the latest concrete aws-cli 2.x release.
#
# The upstream AWS CLI public image publishes concrete `2.x.y` tags and `latest`;
# it does NOT publish a floating `2` tag. So this script DISCOVERS the highest
# concrete `2.x.y` tag, resolves that tag's top-level index (OCI image index or
# Docker manifest list), verifies the index includes BOTH linux/amd64 and
# linux/arm64, computes the digest from the exact verified bytes, and only then
# rewrites the pins. It fails closed on any missing tag, digest, or platform, and
# prints ONLY the pinned image@sha256 line on success.
#
# Resolver tools (exact reviewed versions — this script NEVER installs a tool):
#   - crane          v0.21.7  (github.com/google/go-containerregistry)  [preferred]
#   - docker buildx  v0.34.1  (github.com/docker/buildx)                [fallback]
# These are the tested versions, not floors; newer releases are not auto-trusted.
# Tag discovery uses `crane ls` (crane path) or the ECR Public registry HTTP v2
# API via curl (docker path). Force a path with UPDATE_IMAGE_TOOL=crane|docker.
set -euo pipefail
IFS=$'\n\t'

REPO="public.ecr.aws/aws-cli/aws-cli"
REGISTRY_PATH="aws-cli/aws-cli"          # path under public.ecr.aws for the v2 API
TAG_RE='^2\.[0-9]+\.[0-9]+$'             # concrete 2.x.y only; NOT the floating "2"

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# Overridable for tests; default to the committed pins next to the seeder.
IMAGE_TXT="${IMAGE_TXT:-${HERE}/../image.txt}"
DOCKERFILE="${SEEDER_DOCKERFILE:-${HERE}/../Dockerfile}"

# Pick the resolver. UPDATE_IMAGE_TOOL forces one (also used by the tests).
TOOL="${UPDATE_IMAGE_TOOL:-}"
if [ -z "$TOOL" ]; then
  if command -v crane >/dev/null 2>&1; then
    TOOL="crane"
  elif command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then
    TOOL="docker"
  else
    cat >&2 <<'EOF'
update-image.sh: need a reviewed resolver to pin a multi-platform index digest.
Tested with these exact versions:
  - crane          v0.21.7  (github.com/google/go-containerregistry)
  - docker buildx  v0.34.1  (github.com/docker/buildx)
Install one of those exact reviewed versions from its official distribution. This
script never installs a tool and never pins to an unverified digest.
EOF
    exit 2
  fi
fi

# --- tag discovery -----------------------------------------------------------
list_tags_registry_api() {
  # ECR Public anonymous token, then the v2 tags list. curl + python3 only.
  local token
  token=$(curl -fsSL "https://public.ecr.aws/token/" \
    | python3 -c 'import sys, json; print(json.load(sys.stdin).get("token", ""))') || return 1
  [ -n "$token" ] || return 1
  curl -fsSL -H "Authorization: Bearer ${token}" \
    "https://public.ecr.aws/v2/${REGISTRY_PATH}/tags/list" \
    | python3 -c 'import sys, json; [print(t) for t in json.load(sys.stdin).get("tags", [])]'
}

resolve_concrete_tag() {
  local tags
  if [ "$TOOL" = "crane" ]; then
    tags=$(crane ls "$REPO") || return 1
  else
    tags=$(list_tags_registry_api) || return 1
  fi
  printf '%s\n' "$tags" | grep -E "$TAG_RE" | sort -V | tail -n 1
}

CONCRETE_TAG=$(resolve_concrete_tag || true)
if [ -z "$CONCRETE_TAG" ]; then
  echo "update-image.sh: no concrete ${REPO} 2.x.y tag found (the upstream image publishes no floating '2' tag)" >&2
  exit 3
fi
RESOLVE_REF="${REPO}:${CONCRETE_TAG}"

# --- digest + platform verification -----------------------------------------
# fetch_raw_index prints the RAW top-level index/manifest-list bytes. Both
# resolvers return the exact stored bytes, so their sha256 equals the registry
# index digest.
fetch_raw_index() {
  local ref="$1"
  if [ "$TOOL" = "crane" ]; then
    crane manifest "$ref"
  else
    docker buildx imagetools inspect "$ref" --raw
  fi
}

# verify_index_and_digest reads the raw index on STDIN, verifies it is a
# multi-platform index covering linux/amd64 + linux/arm64, and prints the digest
# computed from those exact bytes. Any problem exits non-zero (fail closed).
verify_index_and_digest() {
  python3 -c '
import sys, json, hashlib

raw = sys.stdin.buffer.read()
if not raw:
    sys.stderr.write("update-image.sh: empty manifest from registry tool\n")
    sys.exit(1)

digest = "sha256:" + hashlib.sha256(raw).hexdigest()

try:
    doc = json.loads(raw)
except Exception as exc:  # noqa: BLE001
    sys.stderr.write("update-image.sh: manifest is not valid JSON: %s\n" % exc)
    sys.exit(1)

index_media = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
media = doc.get("mediaType", "")
manifests = doc.get("manifests")
if media not in index_media or not isinstance(manifests, list):
    sys.stderr.write(
        "update-image.sh: tag is not a top-level multi-platform index "
        "(mediaType=%r)\n" % media
    )
    sys.exit(1)

plats = set()
for entry in manifests:
    plat = entry.get("platform") or {}
    os_ = plat.get("os")
    arch = plat.get("architecture")
    if not os_ or os_ == "unknown" or not arch:
        continue
    plats.add("%s/%s" % (os_, arch))

required = {"linux/amd64", "linux/arm64"}
missing = required - plats
if missing:
    sys.stderr.write(
        "update-image.sh: index missing required platforms %s (have: %s)\n"
        % (sorted(missing), sorted(plats))
    )
    sys.exit(1)

sys.stdout.write(digest + "\n")
'
}

# Resolve + verify + digest in one pipeline: the tool's stdout is the JSON
# parser's stdin. A failure anywhere (tool error, non-index, missing platform,
# bad JSON) leaves DIGEST unset and aborts before any file is rewritten.
DIGEST=""
if ! DIGEST=$(fetch_raw_index "$RESOLVE_REF" | verify_index_and_digest); then
  echo "update-image.sh: failed to resolve/verify a multi-platform index for ${RESOLVE_REF}" >&2
  exit 3
fi

case "$DIGEST" in
  sha256:????????????????????????????????????????????????????????????????) : ;;
  *) echo "update-image.sh: invalid digest '${DIGEST}' for ${RESOLVE_REF}" >&2; exit 3 ;;
esac

NEW_LINE="${REPO}:${CONCRETE_TAG}@${DIGEST}"

# --- atomic two-file update --------------------------------------------------
# Validate BOTH targets and stage BOTH complete outputs before replacing either;
# on any validation error, neither file changes. The only residual non-atomicity
# is a crash between the two final renames (POSIX has no multi-file rename).
if [ ! -f "$DOCKERFILE" ]; then
  echo "update-image.sh: Dockerfile not found: ${DOCKERFILE}" >&2
  exit 4
fi
for target in "$IMAGE_TXT" "$DOCKERFILE"; do
  target_dir=$(dirname -- "$target")
  if [ ! -d "$target_dir" ] || [ ! -w "$target_dir" ]; then
    echo "update-image.sh: destination directory not writable: ${target_dir}" >&2
    exit 4
  fi
  if [ -e "$target" ] && [ ! -w "$target" ]; then
    echo "update-image.sh: destination not writable: ${target}" >&2
    exit 4
  fi
done

# Stage both outputs in temp files in the SAME directory (atomic rename target).
IMG_TMP=$(mktemp "${IMAGE_TXT}.XXXXXX")
DOCKERFILE_TMP=$(mktemp "${DOCKERFILE}.XXXXXX")
trap 'rm -f -- "$IMG_TMP" "$DOCKERFILE_TMP"' EXIT

printf '%s\n' "$NEW_LINE" > "$IMG_TMP"

# Rewrite EXACTLY the one `FROM <repo>` line; require exactly one such line, else
# fail without touching either real file. Guards against zero-FROM (nothing to
# pin) and multiple-FROM (ambiguous) Dockerfiles.
if ! NEW_REF="$NEW_LINE" REPO="$REPO" DOCKERFILE_SRC="$DOCKERFILE" \
     python3 - "$DOCKERFILE_TMP" <<'PY'; then
import os, re, sys

out_path = sys.argv[1]
new_ref = os.environ["NEW_REF"]
repo = os.environ["REPO"]
with open(os.environ["DOCKERFILE_SRC"], "r", encoding="utf-8") as fh:
    src = fh.read()
pattern = r"^FROM " + re.escape(repo) + r"(?:[:@ ].*)?$"
matches = re.findall(pattern, src, flags=re.M)
if len(matches) != 1:
    sys.stderr.write(
        "update-image.sh: expected exactly one 'FROM %s ...' line, found %d\n"
        % (repo, len(matches)))
    sys.exit(1)
new_src = re.sub(pattern, "FROM " + new_ref, src, count=1, flags=re.M)
with open(out_path, "w", encoding="utf-8") as fh:
    fh.write(new_src)
PY
  echo "update-image.sh: Dockerfile FROM rewrite failed; no files changed" >&2
  exit 4
fi

# Both outputs staged and valid → replace both.
mv -f -- "$IMG_TMP" "$IMAGE_TXT"
mv -f -- "$DOCKERFILE_TMP" "$DOCKERFILE"
trap - EXIT

# Output ONLY the pinned image@sha256 reference.
printf '%s\n' "$NEW_LINE"
