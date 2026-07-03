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

echo "------------"
printf 'pass=%d fail=%d\n' "$PASS" "$FAIL"
[ "$FAIL" = "0" ]
