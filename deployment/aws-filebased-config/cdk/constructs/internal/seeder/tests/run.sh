#!/usr/bin/env bash
# tests/run.sh — portable seeder integration tests.
#
# Requires: bash, python3 (with PyYAML), mktemp, mv.
# Does NOT require Docker. The `aws` CLI is mocked via a PATH shim that
# copies a fixture file in lieu of an S3 download.
set -euo pipefail
IFS=$'\n\t'

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "${HERE}/.." && pwd)
SEEDER="${ROOT}/seeder.sh"
export FIXTURES_DIR="${HERE}/fixtures"
export PATH="${FIXTURES_DIR}/bin:${PATH}"

# Sandboxed EFS mount per test.
WORKDIR=$(mktemp -d -- "${TMPDIR:-/tmp}/seeder-tests.XXXXXX")
trap 'rm -rf -- "$WORKDIR"' EXIT

PASS=0; FAIL=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
nope() { printf '  \033[31mFAIL\033[0m %s :: %s\n' "$1" "$2"; FAIL=$((FAIL+1)); }

# Helper: assert exit code AND that stdout is exactly one valid JSON line
# whose `exit` field matches.
assert_run() {
  local name="$1" want_exit="$2" want_reason="$3"; shift 3
  local efs="${WORKDIR}/case-${name}"
  mkdir -p -- "$efs"
  if [ "${PRESEED:-}" != "" ]; then
    cp -- "$PRESEED" "${efs}/bridge.yaml"
  fi
  local out rc=0
  set +e
  out=$(env \
        MODE="${MODE:-SeedOnce}" \
        ASSET_S3_URI="${ASSET_S3_URI:-s3://test/bridge.yaml}" \
        EFS_TARGET_PATH="${efs}/bridge.yaml" \
        EXPECTED_HASH="${EXPECTED_HASH:-}" \
        LOG_STREAM_PREFIX="${LOG_STREAM_PREFIX:-test}" \
        bash "$SEEDER" 2>&1)
  rc=$?
  set -e
  unset PRESEED MODE ASSET_S3_URI EXPECTED_HASH LOG_STREAM_PREFIX

  if [ "$rc" -ne "$want_exit" ]; then
    nope "$name" "want exit $want_exit got $rc; output: $out"
    return
  fi
  # Must be exactly one line of valid JSON. $(...) strips the trailing
  # newline, so a single-line outcome captures with NO embedded newline.
  case "$out" in
    *$'\n'*) nope "$name" "expected exactly 1 log line, got multiple :: $out"; return ;;
    "")      nope "$name" "expected one log line, got empty output"; return ;;
  esac
  if ! OUT="$out" python3 - "$want_exit" "$want_reason" <<'PY'; then
import json, os, sys
want_exit = int(sys.argv[1])
want_reason = sys.argv[2]
line = os.environ["OUT"].strip()
obj = json.loads(line)
for k in ("level", "ts", "mode", "reason", "exit"):
    assert k in obj, "missing " + k
assert obj["exit"] == want_exit,    "exit "   + str(obj["exit"])   + " != " + str(want_exit)
assert obj["reason"] == want_reason, "reason " + str(obj["reason"]) + " != " + want_reason
PY
    nope "$name" "JSON shape/reason mismatch :: $out"
    return
  fi
  ok "$name"
}

echo "seeder tests"
echo "============"

# 1) SeedOnce — fresh seed when target absent.
MODE=SeedOnce ASSET_S3_URI=s3://test/bridge.yaml \
  assert_run "SeedOnce-fresh-seed" 0 "seeded"
# Verify the file exists on the EFS sandbox.
[ -f "${WORKDIR}/case-SeedOnce-fresh-seed/bridge.yaml" ] \
  || { nope "SeedOnce-fresh-seed-file" "target file not created"; }

# 2) SeedOnce — hash match: pre-seed with semantically-equivalent YAML.
PRESEED="${FIXTURES_DIR}/bridge.yaml.equiv" \
MODE=SeedOnce ASSET_S3_URI=s3://test/bridge.yaml \
  assert_run "SeedOnce-hash-match" 0 "hash_match"

# 3) AbortDeploy — mismatch: pre-seed with different content.
EXPECTED=$(python3 -c '
import hashlib, yaml
with open("'"${FIXTURES_DIR}/bridge.yaml"'", "rb") as f: d = f.read()
canon = yaml.safe_dump(yaml.safe_load(d), sort_keys=True, default_flow_style=False)
print(hashlib.sha256(canon.encode()).hexdigest())')
PRESEED="${FIXTURES_DIR}/bridge.yaml.alt" \
MODE=AbortDeploy ASSET_S3_URI=s3://test/bridge.yaml EXPECTED_HASH="$EXPECTED" \
  assert_run "AbortDeploy-mismatch" 10 "hash_mismatch"

# 4) Unparseable YAML asset → exit 30.
MODE=SeedOnce ASSET_S3_URI=s3://test/bridge.yaml.bad \
  assert_run "SeedOnce-yaml-unparseable" 30 "yaml_unparseable"

# 5) AdoptValid — hash match: worker sees the exact synth-time config.
PRESEED="${FIXTURES_DIR}/bridge.yaml.equiv" \
MODE=AdoptValid ASSET_S3_URI=s3://test/bridge.yaml \
  assert_run "AdoptValid-hash-match" 0 "hash_match"

# 6) AdoptValid — hash drift: the control node (admin config-txn commit)
#    rewrote bridge.yaml. A worker MUST adopt it (exit 0), not AbortDeploy,
#    so scale-out / crash-replacement keeps working after any admin edit.
PRESEED="${FIXTURES_DIR}/bridge.yaml.alt" \
MODE=AdoptValid ASSET_S3_URI=s3://test/bridge.yaml \
  assert_run "AdoptValid-adopts-drift" 0 "adopted_existing_config"

# 7) AdoptValid — target absent: a worker with no config bridges nothing.
MODE=AdoptValid ASSET_S3_URI=s3://test/bridge.yaml \
  assert_run "AdoptValid-target-absent" 10 "target_absent"

# 8) AdoptValid — unparseable target: fail closed rather than adopt garbage.
PRESEED="${FIXTURES_DIR}/bridge.yaml.bad" \
MODE=AdoptValid ASSET_S3_URI=s3://test/bridge.yaml \
  assert_run "AdoptValid-target-unparseable" 30 "yaml_unparseable"

echo
echo "update-image.sh tests"
echo "====================="

UPDATE_IMAGE="${ROOT}/scripts/update-image.sh"

# --- manifest fixtures -------------------------------------------------------
# OCI image index with amd64 + arm64 (happy path); the unknown/unknown
# attestation entry must be ignored.
cat > "${WORKDIR}/idx-ok.json" <<'JSON'
{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
 {"platform":{"os":"linux","architecture":"amd64"}},
 {"platform":{"os":"linux","architecture":"arm64","variant":"v8"}},
 {"platform":{"os":"unknown","architecture":"unknown"}}
]}
JSON
# Docker manifest list (what real `docker buildx imagetools inspect --raw`
# returns for aws-cli) with amd64 + arm64 — must also be accepted.
cat > "${WORKDIR}/idx-dockerlist.json" <<'JSON'
{"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[
 {"platform":{"os":"linux","architecture":"amd64"}},
 {"platform":{"os":"linux","architecture":"arm64"}}
]}
JSON
# Index missing arm64 → must fail closed.
cat > "${WORKDIR}/idx-noarm.json" <<'JSON'
{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
 {"platform":{"os":"linux","architecture":"amd64"}}
]}
JSON
# A single-arch image manifest (not a multi-platform index) → must fail closed.
cat > "${WORKDIR}/idx-single.json" <<'JSON'
{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}
JSON
# Malformed JSON → must fail closed.
printf '{not valid json' > "${WORKDIR}/idx-bad.json"

# Pin the released seeder, never its upstream base or a mutable tag.
SEEDER_REPO="docker.io/mariotoffia/gobridge-seeder"
index_ref() {
  printf '%s@sha256:%s\n' "$SEEDER_REPO" \
    "$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1")"
}

# run_ui tool name manifest ref want_exit [destination]
run_ui() {
  local tool="$1" name="$2" manifest="$3" ref="$4" want_exit="$5" destination="${6:-normal}"
  local img="${WORKDIR}/ui-${tool}-${name}-image.txt"
  local dfile="${WORKDIR}/ui-${tool}-${name}-Dockerfile"
  local toollog="${WORKDIR}/ui-${tool}-${name}-tool.log"
  printf 'SENTINEL-OLD\n' > "$img"
  printf 'FROM public.ecr.aws/aws-cli/aws-cli:2.0.0@sha256:0000\n' > "$dfile"
  local img_sum_before dfile_sum_before
  img_sum_before=$(cksum < "$img")
  dfile_sum_before=$(cksum < "$dfile")
  case "$destination" in
    readonly) chmod 0444 "$img" ;;
    missing-dir) img="${WORKDIR}/missing-${name}/image.txt" ;;
    directory) img="${WORKDIR}/directory-${name}"; mkdir "$img" ;;
  esac
  : > "$toollog"
  local out rc=0
  set +e
  out=$(UPDATE_IMAGE_TOOL="$tool" FAKE_TOOL_LOG="$toollog" \
        FAKE_CRANE_MANIFEST="$manifest" FAKE_DOCKER_MANIFEST="$manifest" \
        SEEDER_IMAGE="$ref" IMAGE_TXT="$img" SEEDER_DOCKERFILE="$dfile" \
        bash "$UPDATE_IMAGE" 2>/dev/null)
  rc=$?
  set -e
  [ "$destination" = readonly ] && chmod 0644 "$img"
  local label="ui-${tool}-${name}"
  if [ "$rc" -ne "$want_exit" ]; then
    nope "$label" "want exit $want_exit got $rc; out: $out; log: $(cat "$toollog" 2>/dev/null)"; return
  fi
  if [ "$rc" = "0" ]; then
    if [ "$tool" = "crane" ]; then
      grep -qxF "crane manifest ${ref}" "$toollog" \
        || { nope "$label" "resolver did not use the pinned reference"; return; }
    else
      grep -qxF "docker buildx imagetools inspect ${ref} --raw" "$toollog" \
        || { nope "$label" "resolver did not use the pinned reference"; return; }
    fi
    if [ "$out" != "$ref" ] || [ "$(cat "$img")" != "$ref" ]; then
      nope "$label" "output and image.txt must equal the released reference"; return
    fi
  else
    if [ -f "$img" ] && [ "$(cksum < "$img")" != "$img_sum_before" ]; then
      nope "$label" "image.txt changed on a fail-closed run"; return
    fi
  fi
  if [ "$(cksum < "$dfile")" != "$dfile_sum_before" ]; then
    nope "$label" "upstream Dockerfile must not be changed when pinning the built image"; return
  fi
  ok "$label"
}

for tool in crane docker; do
  run_ui "$tool" "ok" "${WORKDIR}/idx-ok.json" "$(index_ref "${WORKDIR}/idx-ok.json")" 0
  run_ui "$tool" "missing-arm64" "${WORKDIR}/idx-noarm.json" "$(index_ref "${WORKDIR}/idx-noarm.json")" 3
  run_ui "$tool" "not-an-index" "${WORKDIR}/idx-single.json" "$(index_ref "${WORKDIR}/idx-single.json")" 3
  run_ui "$tool" "malformed-json" "${WORKDIR}/idx-bad.json" "$(index_ref "${WORKDIR}/idx-bad.json")" 3
  run_ui "$tool" "digest-mismatch" "${WORKDIR}/idx-ok.json" "$(index_ref "${WORKDIR}/idx-noarm.json")" 3
  run_ui "$tool" "fetch-failed" "${WORKDIR}/missing.json" "$(index_ref "${WORKDIR}/idx-ok.json")" 3
  run_ui "$tool" "mutable-tag" "${WORKDIR}/idx-ok.json" "$SEEDER_REPO:latest" 2
  run_ui "$tool" "missing-ref" "${WORKDIR}/idx-ok.json" "" 2
done
run_ui "docker" "docker-manifest-list" "${WORKDIR}/idx-dockerlist.json" "$(index_ref "${WORKDIR}/idx-dockerlist.json")" 0
run_ui "bogus" "invalid-tool" "${WORKDIR}/idx-ok.json" "$(index_ref "${WORKDIR}/idx-ok.json")" 2
run_ui "crane" "missing-dir" "${WORKDIR}/idx-ok.json" "$(index_ref "${WORKDIR}/idx-ok.json")" 4 missing-dir
run_ui "crane" "directory-pin" "${WORKDIR}/idx-ok.json" "$(index_ref "${WORKDIR}/idx-ok.json")" 4 directory
if [ "$(id -u)" != "0" ]; then
  run_ui "crane" "readonly-pin" "${WORKDIR}/idx-ok.json" "$(index_ref "${WORKDIR}/idx-ok.json")" 4 readonly
fi

echo "------------"
printf 'pass=%d fail=%d\n' "$PASS" "$FAIL"
python3 "${HERE}/ddb_cases.py" || FAIL=$((FAIL+1))
[ "$FAIL" = "0" ]
