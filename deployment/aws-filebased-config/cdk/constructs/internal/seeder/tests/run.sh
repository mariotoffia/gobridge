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

# --- tag-list fixtures (crane `ls` lines + registry v2 JSON) ------------------
# "concrete" advertises the mutable 2 and latest plus real 2.x.y tags; the
# script must pick the HIGHEST concrete 2.x.y (2.35.24), never the mutable 2.
printf '%s\n' latest 2 1.0.0 2.4.0 2.35.9 2.35.24 > "${WORKDIR}/tags-concrete.txt"
cat > "${WORKDIR}/tags-concrete.json" <<'JSON'
{"tags":["latest","2","1.0.0","2.4.0","2.35.9","2.35.24"]}
JSON
# "none" advertises only mutable/floating tags → no concrete 2.x.y exists.
printf '%s\n' latest 2 > "${WORKDIR}/tags-none.txt"
cat > "${WORKDIR}/tags-none.json" <<'JSON'
{"tags":["latest","2"]}
JSON

# run_ui tool name manifest tagset want_exit want_written [expect_tag] [df_shape]
# df_shape: one-from (default) | zero-from | two-from | readonly | missing-dir
run_ui() {
  local tool="$1" name="$2" manifest="$3" tagset="$4" want_exit="$5" want_written="$6" expect_tag="${7:-}" df_shape="${8:-one-from}"
  local img="${WORKDIR}/ui-${tool}-${name}-image.txt"
  local dfile="${WORKDIR}/ui-${tool}-${name}-Dockerfile"
  local toollog="${WORKDIR}/ui-${tool}-${name}-tool.log"
  printf 'SENTINEL-OLD\n' > "$img"
  local df_present=yes
  case "$df_shape" in
    one-from)  printf 'FROM public.ecr.aws/aws-cli/aws-cli:2.0.0@sha256:0000\nRUN true\n' > "$dfile" ;;
    zero-from) printf 'FROM alpine:3.20\nRUN true\n' > "$dfile" ;;
    two-from)  printf 'FROM public.ecr.aws/aws-cli/aws-cli:2.0.0@sha256:0\nFROM public.ecr.aws/aws-cli/aws-cli:2.0.1@sha256:1\n' > "$dfile" ;;
    readonly)  printf 'FROM public.ecr.aws/aws-cli/aws-cli:2.0.0@sha256:0000\nRUN true\n' > "$dfile"; chmod 0444 "$dfile" ;;
    missing-dir) dfile="${WORKDIR}/ui-${tool}-${name}-nodir/Dockerfile"; df_present=no ;;
  esac
  local img_sum_before dfile_sum_before=""
  img_sum_before=$(cksum < "$img")
  [ "$df_present" = yes ] && dfile_sum_before=$(cksum < "$dfile")
  : > "$toollog"
  local out rc=0
  set +e
  out=$(UPDATE_IMAGE_TOOL="$tool" FAKE_TOOL_LOG="$toollog" \
        FAKE_CRANE_MANIFEST="$manifest" FAKE_DOCKER_MANIFEST="$manifest" \
        FAKE_CRANE_TAGS="${WORKDIR}/tags-${tagset}.txt" \
        FAKE_REGISTRY_TAGS="${WORKDIR}/tags-${tagset}.json" \
        IMAGE_TXT="$img" SEEDER_DOCKERFILE="$dfile" \
        bash "$UPDATE_IMAGE" 2>/dev/null)
  rc=$?
  set -e
  [ "$df_shape" = readonly ] && chmod 0644 "$dfile" 2>/dev/null
  local label="ui-${tool}-${name}"
  if [ "$rc" -ne "$want_exit" ]; then
    nope "$label" "want exit $want_exit got $rc; out: $out; log: $(cat "$toollog" 2>/dev/null)"; return
  fi
  if [ "$want_written" = "yes" ]; then
    case "$out" in *$'\n'*) nope "$label" "stdout must be one line, got: $out"; return ;; esac
    local expect_ref="public.ecr.aws/aws-cli/aws-cli:${expect_tag}"
    # The resolver must receive the EXACT concrete reference.
    if [ "$tool" = "crane" ]; then
      grep -qF "crane manifest ${expect_ref}" "$toollog" \
        || { nope "$label" "crane resolver ref != ${expect_ref}; log: $(cat "$toollog")"; return; }
    else
      grep -qF "docker buildx imagetools inspect ${expect_ref} --raw" "$toollog" \
        || { nope "$label" "docker resolver ref != ${expect_ref}; log: $(cat "$toollog")"; return; }
    fi
    # The digest must be computed from the exact bytes that reached the verifier
    # on stdin (proves the JSON was piped in, not clobbered).
    local expect_digest
    expect_digest="sha256:$(python3 -c 'import sys,hashlib;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$manifest")"
    if [ "$out" != "${expect_ref}@${expect_digest}" ]; then
      nope "$label" "stdout '$out' != ${expect_ref}@${expect_digest}"; return
    fi
    if [ "$(cat "$img")" != "$out" ]; then
      nope "$label" "image.txt content != printed pinned ref"; return
    fi
    if [ "$(grep -m1 '^FROM ' "$dfile")" != "FROM $out" ]; then
      nope "$label" "Dockerfile FROM not synced to the pinned ref"; return
    fi
  else
    # Fail closed: NEITHER file may change (unchanged checksums).
    if [ "$(cksum < "$img")" != "$img_sum_before" ]; then
      nope "$label" "image.txt changed on a fail-closed run"; return
    fi
    if [ "$df_present" = yes ] && [ "$(cksum < "$dfile")" != "$dfile_sum_before" ]; then
      nope "$label" "Dockerfile changed on a fail-closed run"; return
    fi
  fi
  ok "$label"
}

# Both resolver paths: crane (crane ls + crane manifest) and docker buildx
# (registry API tag list + imagetools inspect --raw). One-FROM Dockerfile.
for tool in crane docker; do
  run_ui "$tool" "ok"              "${WORKDIR}/idx-ok.json"     concrete 0 yes 2.35.24
  run_ui "$tool" "missing-arm64"   "${WORKDIR}/idx-noarm.json"  concrete 3 no
  run_ui "$tool" "not-an-index"    "${WORKDIR}/idx-single.json" concrete 3 no
  run_ui "$tool" "malformed-json"  "${WORKDIR}/idx-bad.json"    concrete 3 no
  run_ui "$tool" "rejects-mutable-2" "${WORKDIR}/idx-ok.json"   none     3 no
done
# The docker path must also accept a Docker manifest-list mediaType.
run_ui "docker" "docker-manifest-list" "${WORKDIR}/idx-dockerlist.json" concrete 0 yes 2.35.24

# Atomicity: the digest resolves, but a bad Dockerfile target must abort BEFORE
# either file is rewritten (exit 4, both checksums unchanged).
run_ui "crane" "atomic-zero-from"   "${WORKDIR}/idx-ok.json" concrete 4 no "" zero-from
run_ui "crane" "atomic-two-from"    "${WORKDIR}/idx-ok.json" concrete 4 no "" two-from
run_ui "crane" "atomic-missing-dir" "${WORKDIR}/idx-ok.json" concrete 4 no "" missing-dir
# The read-only-file guard only holds for a non-root user (root ignores 0444).
if [ "$(id -u)" != "0" ]; then
  run_ui "crane" "atomic-readonly-df" "${WORKDIR}/idx-ok.json" concrete 4 no "" readonly
fi

echo "------------"
printf 'pass=%d fail=%d\n' "$PASS" "$FAIL"
[ "$FAIL" = "0" ]
