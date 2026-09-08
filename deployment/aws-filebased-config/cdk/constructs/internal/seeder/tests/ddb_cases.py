"""Portable DynamoDB cases using the existing PATH-based AWS fixture."""
import copy
import hashlib
import json
import os
from decimal import Decimal
from pathlib import Path
import subprocess
import tempfile

HERE = Path(__file__).resolve().parent
SCRIPT = HERE.parent / "seeder-ddb.sh"
ASSET = (HERE / "fixtures/bridge.json").read_bytes()
DOCUMENT = json.loads(ASSET)


def row(version=7, drift=False):
    doc = copy.deepcopy(DOCUMENT)
    doc["version"] = version
    if drift:
        doc["bridge"]["log_level"] = "debug"
    return {"PK": {"S": "config#test-bridge"}, "SK": {"S": "current"},
            "data": {"S": json.dumps(doc)}, "version": {"N": str(version)}}


CASES = []


def case(name, mode, current, code, reason, puts=0, **kwargs):
    CASES.append((name, mode, current, code, reason, puts, kwargs))


case("seed-absent", "SeedOnce", None, 0, "seeded", 1, version=1)
case("overwrite-absent", "Overwrite", None, 0, "seeded", 1, version=1)
case("seed-existing", "SeedOnce", row(), 0, "hash_match")
case("seed-keeps-drift", "SeedOnce", row(drift=True), 0, "hash_mismatch_kept_existing")
case("abort-match-ignores-version", "AbortDeploy", row(), 0, "hash_match")
case("abort-drift", "AbortDeploy", row(drift=True), 10, "hash_mismatch")
case("abort-absent", "AbortDeploy", None, 10, "target_absent")
case("adopt-match", "AdoptValid", row(), 0, "hash_match")
case("adopt-drift", "AdoptValid", row(drift=True), 0, "adopted_existing_config")
case("adopt-absent", "AdoptValid", None, 10, "target_absent")
case("overwrite-bumps-current-not-yaml", "Overwrite", row(drift=True), 0, "seeded", 1, version=8)
case("overwrite-same-data-bumps", "Overwrite", row(), 0, "seeded", 1, version=8)
case("cas-reloads-conflict", "Overwrite", row(), 0, "seeded", 2, scenario="conflict-once", version=9)
case("cas-bounded", "Overwrite", row(), 60, "cas_exhausted", 3, scenario="conflict-always")
case("seed-concurrent-controls", "SeedOnce", None, 0, "hash_match", 1, scenario="seed-race", version=2)
case("read-error-not-absence", "SeedOnce", None, 60, "dynamodb_read_failed", scenario="read-error")
case("read-error-not-drift", "AbortDeploy", row(), 60, "dynamodb_read_failed", scenario="read-error")
case("write-error-not-conflict", "Overwrite", row(), 60, "dynamodb_write_failed", 1, scenario="write-error")
case("throttle-not-conflict", "Overwrite", row(), 60, "dynamodb_write_failed", 1, scenario="write-throttle")
case("invalid-api-response", "AdoptValid", row(), 30, "invalid_item", scenario="invalid-response")
case("s3-failure", "SeedOnce", None, 20, "s3_download_failed", uri="s3://test/missing.json")
case("asset-integrity", "SeedOnce", None, 30, "asset_hash_mismatch", expected="0" * 64)
case("invalid-mode", "unsafe", row(), 1, "invalid_mode")
case("missing-table", "SeedOnce", None, 1, "missing_env", table="")
case("missing-hash", "SeedOnce", None, 1, "missing_env", expected="")
case("invalid-asset-json", "SeedOnce", None, 30, "invalid_asset", asset=b'{bad')
case("invalid-asset-shape", "SeedOnce", None, 30, "invalid_asset", asset=b'[]')
case("oversize-asset", "SeedOnce", None, 30, "invalid_asset", asset=b' ' * (390 * 1024) + ASSET)
precise = ASSET.replace(b'"table_name":"test-outbox"', b'"table_name":"test-outbox","integer":9007199254740993,"decimal":0.12345678901234567890123456789')
case("preserve-plugin-number-precision", "SeedOnce", None, 0, "seeded", 1, asset=precise, version=1)
precise_row = row()
precise_row["data"]["S"] = precise.replace(b'"version":99', b'"version":7').decode()
case("compare-plugin-number-precision", "AbortDeploy", precise_row, 10, "hash_mismatch",
     asset=precise.replace(b'9007199254740993', b'9007199254740992'))
equivalent_row = copy.deepcopy(precise_row)
equivalent_row["data"]["S"] = equivalent_row["data"]["S"].replace('9007199254740993', '9007199254740993.0')
case("numeric-scale-equivalence", "AbortDeploy", equivalent_row, 0, "hash_match", asset=precise)
boundary = {"bridge": {"id": "test-bridge", "log_level": ""}, "version": 0}
padding = 390 * 1024 - len(json.dumps(boundary, separators=(",", ":")))
boundary["bridge"]["log_level"] = "x" * padding
case("version-growth-size-limit", "Overwrite", row(9007199254740993), 30, "item_too_large",
     asset=json.dumps(boundary, separators=(",", ":")).encode())
legacy = row(0)
del legacy["version"]
legacy["data"]["S"] = json.dumps({k: v for k, v in DOCUMENT.items() if k != "version"})
case("legacy-adopt", "AdoptValid", legacy, 0, "hash_match")
case("legacy-overwrite", "Overwrite", legacy, 0, "seeded", 1, version=1)
for label, stored_version in (("stored", 7), ("legacy", None)):
    existing = row()
    existing["data"]["S"] = json.dumps(DOCUMENT)
    if stored_version is None:
        del existing["version"]
    for mode in ("SeedOnce", "AdoptValid", "AbortDeploy", "Overwrite"):
        writes = mode == "Overwrite"
        options = {"version": (stored_version or 0) + 1} if writes else {}
        case(label + "-version-authority-" + mode, mode, existing, 0,
             "seeded" if writes else "hash_match", int(writes), **options)
stale = row(drift=True)
stale["hash"] = {"S": hashlib.sha256(ASSET).hexdigest()}
case("ignore-stale-hash", "AbortDeploy", stale, 10, "hash_mismatch")
for value in ("-1", "1.5", "1e1", "9223372036854775808"):
    bad = row()
    bad["version"] = {"N": value}
    case("invalid-version-" + value, "Overwrite", bad, 30, "invalid_item")
case("overflow-increment", "Overwrite", row(9223372036854775807), 30, "version_overflow")
case("large-integer-version", "Overwrite", row(9007199254740993), 0, "seeded", 1, version=9007199254740994)
for name, mutate in {
    "version-type": lambda r: r.update(version={"S": "7"}),
    "missing-data": lambda r: r.pop("data"),
    "wrong-data-type": lambda r: r.update(data={"M": {}}),
    "json-syntax": lambda r: r.update(data={"S": "{bad"}),
    "json-array": lambda r: r.update(data={"S": "[]"}),
    "json-empty": lambda r: r.update(data={"S": "{}"}),
    "json-duplicate": lambda r: r.update(data={"S": '{"bridge":{},"bridge":{},"version":7}'}),
    "json-nonfinite": lambda r: r.update(data={"S": '{"bridge":{"id":"test-bridge"},"version":NaN}'}),
    "json-list-shape": lambda r: r.update(data={"S": '{"bridge":{"id":"test-bridge"},"version":7,"routes":"bad"}'}),
    "json-version-bool": lambda r: r.update(data={"S": '{"bridge":{"id":"test-bridge"},"version":true}'}),
    "oversize": lambda r: r.update(data={"S": json.dumps({"bridge": {"id": "x" * (390 * 1024)}, "version": 7})}),
}.items():
    bad = row()
    mutate(bad)
    case("adopt-rejects-" + name, "AdoptValid", bad, 30, "invalid_item")

passed = 0
with tempfile.TemporaryDirectory(prefix="seeder-ddb-tests-") as temp:
    for name, mode, current, code, reason, puts, options in CASES:
        directory = Path(temp) / name
        directory.mkdir()
        state, calls = directory / "state.json", directory / "calls.json"
        asset = options.get("asset", ASSET)
        asset_path = directory / "asset.json"
        if "asset" in options:
            asset_path.write_bytes(asset)
        if current is not None:
            state.write_text(json.dumps(current))
        env = dict(os.environ, MODE=mode, TABLE=options.get("table", "test-config"),
                   PK="config#test-bridge", ITEM_S3_URI=options.get("uri", "s3://test/bridge.json"),
                   EXPECTED_HASH=options.get("expected", hashlib.sha256(asset).hexdigest()),
                   ASSET_FIXTURE=str(asset_path) if "asset" in options else "",
                   DDB_STATE=str(state), DDB_CALLS=str(calls), DDB_SCENARIO=options.get("scenario", ""))
        result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True)
        try:
            assert result.returncode == code, (result.returncode, code, result.stdout, result.stderr)
            assert result.stderr == "", result.stderr
            assert len(result.stdout.splitlines()) == 1, result.stdout
            log = json.loads(result.stdout)
            assert {"level", "ts", "mode", "reason", "exit"} <= log.keys(), log
            assert (log["exit"], log["reason"], log["mode"]) == (code, reason, mode), log
            history = json.loads(calls.read_text()) if calls.exists() else []
            assert sum(c["operation"] == "put-item" for c in history) == puts, history
            after = json.loads(state.read_text()) if state.exists() else None
            if puts == 0 or options.get("scenario", "").startswith("write-"):
                assert after == current, "read-only/error path modified current config"
            if "version" in options:
                assert after["version"] == {"N": str(options["version"])}, after
                document = json.loads(after["data"]["S"], parse_float=Decimal)
                assert document["version"] == options["version"], document
                expected_doc = json.loads(asset, parse_float=Decimal)
                assert document.get("stores") == expected_doc.get("stores"), document
            print("  PASS ddb-" + name)
            passed += 1
        except (AssertionError, ValueError) as error:
            print("  FAIL ddb-%s :: %s" % (name, error))
print("dynamodb pass=%d fail=%d" % (passed, len(CASES) - passed))
raise SystemExit(0 if passed == len(CASES) else 1)
