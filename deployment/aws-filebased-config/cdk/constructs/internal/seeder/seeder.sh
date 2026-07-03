#!/usr/bin/env bash
# seeder.sh — GoBridge EFS bridge.yaml seeder / drift checker.
#
# Contract: see README.md (env vars, exit codes, JSON log shape).
# Atomic write: canonical output is created in dirname(EFS_TARGET_PATH)
# via mktemp, then renamed onto EFS_TARGET_PATH (POSIX atomic on same FS).
set -euo pipefail
IFS=$'\n\t'

HANDLED=0
ASSET_CANON=""
DL=""

cleanup_temps() {
  [ -n "${ASSET_CANON:-}" ] && [ -e "${ASSET_CANON:-}" ] && rm -f -- "$ASSET_CANON" || true
  [ -n "${DL:-}" ]          && [ -e "${DL:-}" ]          && rm -f -- "$DL"          || true
}

raw_json_log() {
  # Fallback emitter used when python3 is unavailable. Hardcoded reasons
  # only — never interpolates untrusted strings.
  local level="$1" reason="$2" exit_code="$3"
  local ts
  ts=$(date +%s 2>/dev/null || echo 0)
  printf '{"level":"%s","ts":%s000,"mode":"%s","reason":"%s","exit":%d}\n' \
    "$level" "$ts" "${MODE:-unset}" "$reason" "$exit_code"
}

log() {
  # log <level> <reason> <exit_code> [k1 v1 k2 v2 ...]
  local level="$1" reason="$2" exit_code="$3"
  shift 3
  python3 - "$level" "$reason" "$exit_code" "${MODE:-unset}" "${LOG_STREAM_PREFIX:-}" "$@" <<'PY'
import json, sys, time
a = sys.argv[1:]
level, reason, exit_code, mode, stream = a[:5]
extras = a[5:]
out = {
    "level": level,
    "ts": int(time.time() * 1000),
    "mode": mode,
    "reason": reason,
    "exit": int(exit_code),
}
if stream:
    out["stream"] = stream
i = 0
while i + 1 < len(extras):
    out[extras[i]] = extras[i + 1]
    i += 2
sys.stdout.write(json.dumps(out, separators=(",", ":")) + "\n")
sys.stdout.flush()
PY
}

die() {
  local code="$1" reason="$2"
  shift 2
  HANDLED=1
  if ! log "error" "$reason" "$code" "$@" 2>/dev/null; then
    raw_json_log "error" "$reason" "$code"
  fi
  cleanup_temps
  exit "$code"
}

ok() {
  local reason="$1"
  shift
  HANDLED=1
  log "info" "$reason" 0 "$@"
  cleanup_temps
  exit 0
}

warn_ok() {
  local reason="$1"
  shift
  HANDLED=1
  log "warn" "$reason" 0 "$@"
  cleanup_temps
  exit 0
}

on_exit() {
  local rc=$?
  if [ "$HANDLED" = "0" ]; then
    raw_json_log "error" "internal" 1
    cleanup_temps
    exit 1
  fi
  exit "$rc"
}
trap on_exit EXIT

# --- canonicalizer presence (exit 50) --------------------------------------
if ! command -v python3 >/dev/null 2>&1; then
  HANDLED=1
  raw_json_log "error" "canonicalizer_missing" 50
  exit 50
fi
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  HANDLED=1
  raw_json_log "error" "canonicalizer_missing" 50
  exit 50
fi

# --- env -------------------------------------------------------------------
MODE="${MODE:-SeedOnce}"
case "$MODE" in
  SeedOnce|Overwrite|AbortDeploy|AdoptValid) ;;
  *) die 1 "invalid_mode" "value" "$MODE" ;;
esac

[ -n "${ASSET_S3_URI:-}" ]    || die 1 "missing_env" "var" "ASSET_S3_URI"
[ -n "${EFS_TARGET_PATH:-}" ] || die 1 "missing_env" "var" "EFS_TARGET_PATH"
EXPECTED_HASH="${EXPECTED_HASH:-}"
if [ "$MODE" = "AbortDeploy" ] && [ -z "$EXPECTED_HASH" ]; then
  die 1 "missing_env" "var" "EXPECTED_HASH"
fi

# --- canonicalize helpers --------------------------------------------------
canon_sha() {
  # canon_sha <path>: prints sha256 hex of canonical YAML; exit 2 on parse error.
  python3 - "$1" <<'PY'
import hashlib, sys, yaml
with open(sys.argv[1], "rb") as f:
    data = f.read()
try:
    doc = yaml.safe_load(data) if data.strip() else {}
except yaml.YAMLError as e:
    sys.stderr.write(str(e))
    sys.exit(2)
canon = yaml.safe_dump(doc, sort_keys=True, default_flow_style=False)
sys.stdout.write(hashlib.sha256(canon.encode("utf-8")).hexdigest())
PY
}

canon_write() {
  # canon_write <src> <dst>: writes canonical form to dst, prints sha256 hex.
  python3 - "$1" "$2" <<'PY'
import hashlib, sys, yaml
src, dst = sys.argv[1], sys.argv[2]
with open(src, "rb") as f:
    data = f.read()
try:
    doc = yaml.safe_load(data) if data.strip() else {}
except yaml.YAMLError as e:
    sys.stderr.write(str(e))
    sys.exit(2)
canon = yaml.safe_dump(doc, sort_keys=True, default_flow_style=False)
with open(dst, "w", encoding="utf-8") as f:
    f.write(canon)
sys.stdout.write(hashlib.sha256(canon.encode("utf-8")).hexdigest())
PY
}

# --- writability probe + canon staging dir (exit 40) -----------------------
# AbortDeploy / AdoptValid run as the Worker, which is granted ClientMount
# only on EFS (no ClientWrite). Writing under TARGET_DIR — even a probe or a
# staged canon file — would EACCES and fail the deploy. Skip the probe and
# stage the canonical asset under /tmp; TARGET_DIR is still read for hash
# comparison.
TARGET_DIR=$(dirname -- "$EFS_TARGET_PATH")
if [ "$MODE" = "AbortDeploy" ] || [ "$MODE" = "AdoptValid" ]; then
  STAGE_DIR="/tmp/seeder"
  mkdir -p -- "$STAGE_DIR" 2>/dev/null \
    || die 40 "tmp_not_writable" "path" "$STAGE_DIR"
else
  mkdir -p -- "$TARGET_DIR" 2>/dev/null || die 40 "efs_not_writable" "path" "$TARGET_DIR"
  PROBE=$(mktemp -- "${TARGET_DIR}/.seeder.probe.XXXXXX" 2>/dev/null) \
    || die 40 "efs_not_writable" "path" "$TARGET_DIR"
  rm -f -- "$PROBE"
  STAGE_DIR="$TARGET_DIR"
fi

# --- download asset (exit 20) ----------------------------------------------
DL=$(mktemp -- "/tmp/bridge.asset.XXXXXX" 2>/dev/null) \
  || die 40 "tmp_not_writable" "path" "/tmp"
if ! aws s3 cp "$ASSET_S3_URI" "$DL" >/dev/null 2>&1; then
  die 20 "s3_download_failed" "uri" "$ASSET_S3_URI"
fi

# --- canonicalize asset (exit 30) ------------------------------------------
ASSET_CANON=$(mktemp -- "${STAGE_DIR}/.seeder.asset.XXXXXX" 2>/dev/null) \
  || die 40 "stage_not_writable" "path" "$STAGE_DIR"
if ! ASSET_HASH=$(canon_write "$DL" "$ASSET_CANON" 2>/dev/null); then
  die 30 "yaml_unparseable" "source" "asset"
fi
rm -f -- "$DL"; DL=""

# --- optional sanity check vs EXPECTED_HASH --------------------------------
if [ -n "$EXPECTED_HASH" ] && [ "$EXPECTED_HASH" != "$ASSET_HASH" ]; then
  log "warn" "asset_hash_drift" 0 \
    "expected" "sha256:$EXPECTED_HASH" "actual" "sha256:$ASSET_HASH"
fi

# --- mode dispatch ---------------------------------------------------------
case "$MODE" in
  Overwrite)
    mv -f -- "$ASSET_CANON" "$EFS_TARGET_PATH"
    ASSET_CANON=""
    ok "seeded" "hash" "sha256:$ASSET_HASH" "target" "$EFS_TARGET_PATH"
    ;;

  AbortDeploy)
    if [ ! -e "$EFS_TARGET_PATH" ]; then
      die 10 "target_absent" "expected" "sha256:$ASSET_HASH" "target" "$EFS_TARGET_PATH"
    fi
    if ! EXIST_HASH=$(canon_sha "$EFS_TARGET_PATH" 2>/dev/null); then
      die 30 "yaml_unparseable" "source" "target"
    fi
    if [ "$EXIST_HASH" != "$ASSET_HASH" ]; then
      die 10 "hash_mismatch" "expected" "sha256:$ASSET_HASH" "actual" "sha256:$EXIST_HASH"
    fi
    ok "hash_match" "hash" "sha256:$ASSET_HASH"
    ;;

  AdoptValid)
    # Worker startup gate that COEXISTS with Admin-API hot reconfiguration.
    # A worker cannot write EFS, so it must accept whatever config the
    # control node (CDK seed OR an admin config-txn commit) last wrote — as
    # long as it exists and parses. It never fails on hash drift vs the
    # synth-time asset (that would wedge every scale-out / crash-replacement
    # worker after any admin edit until the next CDK deploy). Absent or
    # unparseable config still fails: a worker with no valid config bridges
    # nothing.
    if [ ! -e "$EFS_TARGET_PATH" ]; then
      die 10 "target_absent" "expected" "sha256:$ASSET_HASH" "target" "$EFS_TARGET_PATH"
    fi
    if ! EXIST_HASH=$(canon_sha "$EFS_TARGET_PATH" 2>/dev/null); then
      die 30 "yaml_unparseable" "source" "target"
    fi
    if [ "$EXIST_HASH" = "$ASSET_HASH" ]; then
      ok "hash_match" "hash" "sha256:$ASSET_HASH"
    else
      warn_ok "adopted_existing_config" \
        "expected" "sha256:$ASSET_HASH" "actual" "sha256:$EXIST_HASH" "target" "$EFS_TARGET_PATH"
    fi
    ;;

  SeedOnce)
    if [ ! -e "$EFS_TARGET_PATH" ]; then
      mv -f -- "$ASSET_CANON" "$EFS_TARGET_PATH"
      ASSET_CANON=""
      ok "seeded" "hash" "sha256:$ASSET_HASH" "target" "$EFS_TARGET_PATH"
    fi
    if ! EXIST_HASH=$(canon_sha "$EFS_TARGET_PATH" 2>/dev/null); then
      die 30 "yaml_unparseable" "source" "target"
    fi
    if [ "$EXIST_HASH" = "$ASSET_HASH" ]; then
      ok "hash_match" "hash" "sha256:$ASSET_HASH"
    else
      warn_ok "hash_mismatch_kept_existing" \
        "expected" "sha256:$ASSET_HASH" "actual" "sha256:$EXIST_HASH"
    fi
    ;;
esac
