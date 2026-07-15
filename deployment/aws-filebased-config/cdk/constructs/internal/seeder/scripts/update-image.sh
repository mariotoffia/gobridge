#!/usr/bin/env bash
# update-image.sh — pin image.txt (and the seeder Dockerfile FROM) to the current
# top-level multi-platform OCI index digest of the aws-cli:2 base image.
#
# It resolves the RAW top-level index once, verifies that index is a genuine
# multi-platform index that includes BOTH linux/amd64 and linux/arm64, computes
# the digest from the exact verified bytes, and only then rewrites the pins. It
# fails closed on a missing/invalid digest or missing platforms, and prints ONLY
# the pinned `image:tag@sha256:<digest>` line on success.
#
# Tooling: `crane` (preferred) or `docker buildx imagetools` — both standard
# container tooling. This script never installs a tool and never pins to an
# unverified digest.
set -euo pipefail
IFS=$'\n\t'

REPO="public.ecr.aws/aws-cli/aws-cli"
TAG="2"

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# Overridable for tests; default to the committed pins next to the seeder.
IMAGE_TXT="${IMAGE_TXT:-${HERE}/../image.txt}"
DOCKERFILE="${SEEDER_DOCKERFILE:-${HERE}/../Dockerfile}"

# Pick the resolver. buildx is needed for the docker path (imagetools).
TOOL=""
if command -v crane >/dev/null 2>&1; then
  TOOL="crane"
elif command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then
  TOOL="docker"
else
  cat >&2 <<'EOF'
update-image.sh: need `crane` or `docker buildx` to resolve a multi-platform
index digest. Both are standard container tooling; install either from its
official distribution. This script never installs tools and never pins to an
unverified digest.
EOF
  exit 2
fi

# fetch_raw_index prints the RAW top-level manifest/index bytes for a ref. Both
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

# Prefer a concrete 2.x.y tag when the resolver can list it (crane only); fall
# back to the floating tag. The digest below is authoritative either way.
CONCRETE_TAG="$TAG"
if [ "$TOOL" = "crane" ]; then
  resolved=$(crane ls "$REPO" 2>/dev/null | grep -E '^2\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)
  if [ -n "$resolved" ]; then
    CONCRETE_TAG="$resolved"
  fi
fi

RESOLVE_REF="${REPO}:${CONCRETE_TAG}"

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
printf '%s\n' "$NEW_LINE" > "$IMAGE_TXT"

# Keep the seeder Dockerfile FROM in sync with the pin.
if [ -f "$DOCKERFILE" ]; then
  NEW_REF="$NEW_LINE" python3 - "$DOCKERFILE" <<'PY'
import os, re, sys

path = sys.argv[1]
new_ref = os.environ["NEW_REF"]
with open(path, "r", encoding="utf-8") as fh:
    src = fh.read()
new_src = re.sub(r"^FROM .*$", "FROM " + new_ref, src, count=1, flags=re.M)
with open(path, "w", encoding="utf-8") as fh:
    fh.write(new_src)
PY
fi

# Output ONLY the pinned image@sha256 reference.
printf '%s\n' "$NEW_LINE"
