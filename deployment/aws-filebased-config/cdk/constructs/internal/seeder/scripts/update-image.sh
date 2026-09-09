#!/usr/bin/env bash
# Pin an already-published seeder index. The upstream Dockerfile pin is separate.
set -euo pipefail
IFS=$'\n\t'

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
IMAGE_TXT="${IMAGE_TXT:-${HERE}/../image.txt}"
REF="${SEEDER_IMAGE:-}"
if [[ ! "$REF" =~ ^docker\.io/[a-z0-9][a-z0-9_-]*/gobridge-seeder@sha256:[0-9a-f]{64}$ ]]; then
  echo "update-image.sh: SEEDER_IMAGE must be docker.io/<account>/gobridge-seeder@sha256:<64hex>" >&2
  exit 2
fi

TOOL="${UPDATE_IMAGE_TOOL:-}"
if [ -z "$TOOL" ]; then
  if command -v crane >/dev/null 2>&1; then
    TOOL=crane
  elif command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then
    TOOL=docker
  fi
fi
case "$TOOL" in
  crane|docker) ;;
  *) echo "update-image.sh: requires crane or docker buildx (UPDATE_IMAGE_TOOL=crane|docker)" >&2; exit 2 ;;
esac

fetch_index() {
  if [ "$TOOL" = crane ]; then
    crane manifest "$REF"
  else
    docker buildx imagetools inspect "$REF" --raw
  fi
}

# Verify the exact bytes, not a separate lookup that could describe another image.
if ! fetch_index | python3 -c '
import hashlib, json, sys
raw = sys.stdin.buffer.read()
expected = sys.argv[1].rsplit("@", 1)[1]
if "sha256:" + hashlib.sha256(raw).hexdigest() != expected:
    sys.exit("update-image.sh: registry manifest digest does not match SEEDER_IMAGE")
try:
    doc = json.loads(raw)
except (ValueError, UnicodeError) as exc:
    sys.exit("update-image.sh: invalid manifest JSON: %s" % exc)
if not isinstance(doc, dict) or doc.get("mediaType") not in (
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
):
    sys.exit("update-image.sh: expected a multi-platform image index")
entries = doc.get("manifests")
if not isinstance(entries, list) or not all(isinstance(e, dict) for e in entries):
    sys.exit("update-image.sh: malformed image index")
platforms = [
    (e.get("platform") or {}).get("os", "") + "/" +
    (e.get("platform") or {}).get("architecture", "")
    for e in entries if isinstance(e.get("platform"), dict)
]
if platforms.count("linux/amd64") != 1 or platforms.count("linux/arm64") != 1:
    sys.exit("update-image.sh: expected exactly one linux/amd64 and linux/arm64 image")
' "$REF"; then
  echo "update-image.sh: failed to verify published seeder; image.txt unchanged" >&2
  exit 3
fi

if [ ! -d "$(dirname -- "$IMAGE_TXT")" ] || [ ! -w "$(dirname -- "$IMAGE_TXT")" ] ||
  { [ -e "$IMAGE_TXT" ] && { [ ! -f "$IMAGE_TXT" ] || [ ! -w "$IMAGE_TXT" ]; }; }; then
  echo "update-image.sh: destination must be a writable file: $IMAGE_TXT" >&2
  exit 4
fi
TMP=$(mktemp "${IMAGE_TXT}.XXXXXX")
trap 'rm -f -- "$TMP"' EXIT
printf '%s\n' "$REF" > "$TMP"
mv -f -- "$TMP" "$IMAGE_TXT"
trap - EXIT
printf '%s\n' "$REF"
