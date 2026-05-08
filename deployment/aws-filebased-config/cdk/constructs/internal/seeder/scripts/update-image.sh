#!/usr/bin/env bash
# update-image.sh — refresh image.txt with the latest aws-cli:2 digest.
#
# Resolves the digest via `crane` (preferred — no Docker daemon needed) or
# falls back to `docker manifest inspect`. Fails loudly with an install hint
# if neither tool is available.
set -euo pipefail
IFS=$'\n\t'

REPO="public.ecr.aws/aws-cli/aws-cli"
TAG="2"

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
IMAGE_TXT="${HERE}/../image.txt"

resolve_with_crane() {
  crane digest "${REPO}:${TAG}" 2>/dev/null
}

resolve_with_docker() {
  # docker manifest inspect needs experimental features on older Docker; on
  # modern Docker Desktop / Engine 25+ it is GA. We parse the top-level
  # digest from the manifest list / image manifest.
  docker manifest inspect -v "${REPO}:${TAG}" 2>/dev/null \
    | python3 - <<'PY'
import json, sys
data = json.load(sys.stdin)
if isinstance(data, list):
    for entry in data:
        plat = entry.get("Descriptor", {}).get("platform", {})
        if plat.get("os") == "linux" and plat.get("architecture") == "amd64":
            print(entry["Descriptor"]["digest"]); sys.exit(0)
    sys.exit(1)
desc = data.get("Descriptor") or {}
if desc.get("digest"):
    print(desc["digest"]); sys.exit(0)
sys.exit(1)
PY
}

resolve_concrete_tag_with_crane() {
  # We pin tag:digest. Resolve the highest 2.x.y tag that the floating "2"
  # tag points to. crane has no direct query; fall back to listing.
  crane ls "${REPO}" 2>/dev/null \
    | grep -E '^2\.[0-9]+\.[0-9]+$' \
    | sort -V \
    | tail -n 1
}

DIGEST=""
CONCRETE_TAG=""

if command -v crane >/dev/null 2>&1; then
  DIGEST=$(resolve_with_crane || true)
  CONCRETE_TAG=$(resolve_concrete_tag_with_crane || true)
elif command -v docker >/dev/null 2>&1; then
  DIGEST=$(resolve_with_docker || true)
else
  cat >&2 <<'EOF'
update-image.sh: neither `crane` nor `docker` is on PATH.

Install one:
  - crane:  go install github.com/google/go-containerregistry/cmd/crane@latest
  - docker: https://docs.docker.com/get-docker/
EOF
  exit 2
fi

if [ -z "$DIGEST" ]; then
  echo "update-image.sh: failed to resolve digest for ${REPO}:${TAG}" >&2
  exit 3
fi

# Fall back to the floating tag if we couldn't resolve a concrete one.
if [ -z "$CONCRETE_TAG" ]; then
  CONCRETE_TAG="$TAG"
fi

NEW_LINE="${REPO}:${CONCRETE_TAG}@${DIGEST}"
printf '%s\n' "$NEW_LINE" > "$IMAGE_TXT"

# Keep Dockerfile FROM in sync.
DOCKERFILE="${HERE}/../Dockerfile"
if [ -f "$DOCKERFILE" ]; then
  python3 - "$DOCKERFILE" "$NEW_LINE" <<'PY'
import re, sys
path, new_ref = sys.argv[1], sys.argv[2]
with open(path, "r", encoding="utf-8") as f:
    src = f.read()
new_src = re.sub(r"^FROM .*$", f"FROM {new_ref}", src, count=1, flags=re.M)
with open(path, "w", encoding="utf-8") as f:
    f.write(new_src)
PY
fi

echo "updated: ${NEW_LINE}"
