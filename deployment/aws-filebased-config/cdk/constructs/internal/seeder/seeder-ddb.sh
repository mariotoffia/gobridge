#!/usr/bin/env bash
# DynamoDB current-config seeder / read-only drift gate. See README.md.
# Synth has parsed and validated the JSON asset; only Python's stdlib is needed.
set -euo pipefail
if ! command -v python3 >/dev/null 2>&1; then
  printf '{"level":"error","ts":0,"mode":"unset","reason":"canonicalizer_missing","exit":50}\n'
  exit 50
fi
exec python3 -S - <<'PY'
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
from decimal import Decimal

MAX_VERSION = 2**63 - 1
MAX_DATA = 390 * 1024
MODE = os.environ.get("MODE", "SeedOnce")


class Failure(Exception):
    def __init__(self, code, reason, **extra):
        self.code, self.reason, self.extra = code, reason, extra


def terminal(code, reason, level=None, **extra):
    result = dict(level=level or ("error" if code else "info"),
                  ts=int(time.time() * 1000), mode=MODE, reason=reason, exit=code)
    if os.environ.get("LOG_STREAM_PREFIX"):
        result["stream"] = os.environ["LOG_STREAM_PREFIX"]
    result.update(extra)
    print(json.dumps(result, separators=(",", ":")), flush=True)
    raise SystemExit(code)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def invalid_constant(value):
    raise ValueError("nonfinite JSON number")


def decode(raw):
    return json.loads(raw, parse_float=Decimal, parse_constant=invalid_constant,
                      object_pairs_hook=unique_object)


def encode(value, semantic=False):
    # Never round-trip through binary floats. Decimal preserves plugin numbers;
    # Python integers preserve versions above 2**53. Semantic numbers normalize
    # scale without Decimal.normalize(), which can round to the context precision.
    if isinstance(value, dict):
        return "{" + ",".join(json.dumps(k) + ":" + encode(value[k], semantic) for k in sorted(value)) + "}"
    if isinstance(value, list):
        return "[" + ",".join(encode(v, semantic) for v in value) + "]"
    if type(value) is int or isinstance(value, Decimal):
        if not semantic:
            return str(value)
        sign, digits, exponent = Decimal(value).as_tuple()
        coefficient = "".join(str(d) for d in digits).rstrip("0")
        if not coefficient:
            return "0"
        exponent += len(digits) - len(coefficient)
        return ("-" if sign else "") + coefficient + "e" + str(exponent)
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":"))


def digest(document):
    semantic = {k: v for k, v in document.items() if k != "version"}
    return hashlib.sha256(encode(semantic, True).encode("utf-8")).hexdigest()


def config_document(raw):
    if not isinstance(raw, str) or len(raw.encode("utf-8")) > MAX_DATA:
        raise ValueError("invalid data size or type")
    doc = decode(raw)
    if not isinstance(doc, dict) or not isinstance(doc.get("bridge"), dict):
        raise ValueError("config must contain bridge settings")
    if not isinstance(doc["bridge"].get("id"), str) or not doc["bridge"]["id"]:
        raise ValueError("bridge id is missing")
    version = doc.get("version", 0)
    if type(version) is not int or not 0 <= version <= MAX_VERSION:
        raise ValueError("invalid document version")
    # This is a wire-shape gate, not a second implementation of Go validation.
    # The runtime still parses typed plugins and validates the complete graph.
    for key in ("sessions", "receivers", "senders", "bindings", "routes"):
        if key in doc and (not isinstance(doc[key], list) or any(not isinstance(v, dict) for v in doc[key])):
            raise ValueError("invalid config list")
    for key in ("stores", "http", "config_watch"):
        if key in doc and not isinstance(doc[key], dict):
            raise ValueError("invalid config object")
    return doc


def aws(args, code, reason):
    try:
        return subprocess.run(["aws"] + args + ["--no-cli-pager"], capture_output=True,
                              text=True, timeout=70,
                              env=dict(os.environ, AWS_MAX_ATTEMPTS="1", AWS_RETRY_MODE="standard"))
    except (OSError, subprocess.TimeoutExpired):
        raise Failure(code, reason)


def ddb(directory, operation, request):
    path = directory / "request.json"
    path.write_text(json.dumps(request), encoding="utf-8")
    reason = "dynamodb_read_failed" if operation == "get-item" else "dynamodb_write_failed"
    response = aws(["dynamodb", operation, "--cli-input-json", "file://" + str(path),
                    "--output", "json", "--cli-connect-timeout", "10", "--cli-read-timeout", "20"], 60, reason)
    if response.returncode:
        # Classify only the CLI's service-error code, never a substring in an
        # arbitrary message. IAM, throttling and network errors are NOT CAS.
        error = re.search(r"^An error occurred \(([^)]+)\) when calling the \w+ operation:", response.stderr, re.MULTILINE)
        if operation == "put-item" and error and error.group(1) == "ConditionalCheckFailedException":
            return False
        raise Failure(60, reason, **({"error_code": error.group(1)} if error else {}))
    if operation == "put-item":
        return True
    try:
        # AWS CLI prints no bytes for a successful GetItem with no item.
        # Only reach this after checking the exit status; failures are never absence.
        result = decode(response.stdout or "{}")
        if not isinstance(result, dict):
            raise ValueError("invalid response")
        return result
    except (ValueError, TypeError):
        raise Failure(30, "invalid_item")


def read_current(directory, table, key):
    result = ddb(directory, "get-item", dict(TableName=table, Key=key, ConsistentRead=True))
    if "Item" not in result:
        return None, 0
    try:
        item = result["Item"]
        if not isinstance(item, dict) or any(item.get(k) != v for k, v in key.items()):
            raise ValueError("invalid key")
        raw_version = item.get("version", {"N": "0"})
        value = raw_version["N"]
        if set(raw_version) != {"N"} or not isinstance(value, str) or not re.fullmatch(r"[+-]?[0-9]+", value):
            raise ValueError("invalid stored version")
        version = int(value)
        if not 0 <= version <= MAX_VERSION or set(item["data"]) != {"S"}:
            raise ValueError("invalid item")
        doc = config_document(item["data"]["S"])
        # Match the loader: externally seeded JSON may carry a different
        # version, but the row is always the compare-and-swap authority.
        doc["version"] = version
        return doc, version
    except (KeyError, ValueError, TypeError):
        raise Failure(30, "invalid_item")


def put_current(directory, table, key, asset, current_version):
    if current_version == MAX_VERSION:
        raise Failure(30, "version_overflow")
    document = dict(asset, version=current_version + 1)
    data = encode(document)
    item = dict(key, data={"S": data}, version={"N": str(current_version + 1)})
    # A conservative numeric-size allowance plus exact UTF-8 names/string values
    # bounds the entire item as well as the loader's stricter data payload limit.
    item_size = sum(len(k.encode("utf-8")) + (len(v["S"].encode("utf-8")) if "S" in v else 21) for k, v in item.items())
    if len(data.encode("utf-8")) > MAX_DATA or item_size > 400 * 1024:
        raise Failure(30, "item_too_large")
    request = dict(TableName=table, Item=item)
    if MODE == "SeedOnce":
        request["ConditionExpression"] = "attribute_not_exists(PK)"
    else:
        request.update(ConditionExpression="#v = :expected OR (attribute_not_exists(#v) AND :expected = :zero)",
                       ExpressionAttributeNames={"#v": "version"},
                       ExpressionAttributeValues={":expected": {"N": str(current_version)}, ":zero": {"N": "0"}})
    return ddb(directory, "put-item", request)


def run(directory):
    if MODE not in ("SeedOnce", "Overwrite", "AbortDeploy", "AdoptValid"):
        raise Failure(1, "invalid_mode")
    for name in ("TABLE", "PK", "EXPECTED_HASH", "ITEM_S3_URI"):
        if not os.environ.get(name):
            raise Failure(1, "missing_env", var=name)
    table, pk = os.environ["TABLE"], os.environ["PK"]
    if not pk.startswith("config#") or not 7 < len(pk.encode("utf-8")) <= 2048:
        raise Failure(1, "invalid_pk")
    key = {"PK": {"S": pk}, "SK": {"S": "current"}}
    path = directory / "bridge.json"
    if aws(["s3", "cp", os.environ["ITEM_S3_URI"], str(path)], 20, "s3_download_failed").returncode:
        raise Failure(20, "s3_download_failed")
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != os.environ["EXPECTED_HASH"]:
        raise Failure(30, "asset_hash_mismatch")
    try:
        asset = config_document(raw.decode("utf-8"))
    except (ValueError, TypeError):
        raise Failure(30, "invalid_asset")
    expected = digest(asset)
    for attempt in range(3):
        current, version = read_current(directory, table, key)
        if MODE == "Overwrite" or (MODE == "SeedOnce" and current is None):
            if put_current(directory, table, key, asset, version):
                return "seeded", "info", {"hash": "sha256:" + expected, "version": str(version + 1)}
            if MODE == "Overwrite":
                continue
            # Another control won the absent-row put. Never overwrite it; read
            # and gate the winner exactly as for any other existing config.
            current, version = read_current(directory, table, key)
        if current is None:
            raise Failure(10, "target_absent")
        actual = digest(current)
        if expected == actual:
            return "hash_match", "info", {"hash": "sha256:" + actual}
        hashes = {"expected": "sha256:" + expected, "actual": "sha256:" + actual}
        if MODE == "AbortDeploy":
            raise Failure(10, "hash_mismatch", **hashes)
        reason = "adopted_existing_config" if MODE == "AdoptValid" else "hash_mismatch_kept_existing"
        return reason, "warn", hashes
    raise Failure(60, "cas_exhausted")


try:
    with tempfile.TemporaryDirectory(prefix="seeder-ddb-") as directory:
        reason, level, extra = run(Path(directory))
    terminal(0, reason, level, **extra)
except Failure as error:
    terminal(error.code, error.reason, **error.extra)
except Exception:
    # Do not print raw configs, AWS stderr or tracebacks into CloudWatch.
    terminal(1, "internal")
PY
