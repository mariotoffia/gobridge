# Seeder

Init container that seeds or checks the selected bridge config before the main
GoBridge container starts. ECS gates the main container on seeder `SUCCESS`.
`seeder.sh` handles YAML on EFS; `seeder-ddb.sh` handles the DynamoDB `current`
item. The DynamoDB seeder never mounts EFS, even when SQLite stores need it.

The published `docker.io/mariotoffia/gobridge-seeder` image is pinned by
[image.txt](image.txt). [Dockerfile](Dockerfile) adds PyYAML to a pinned AWS CLI
base, so file and DynamoDB seeding both work without runtime package installs.
The DynamoDB path uses Python's standard library only.
See [MANIFEST.md](MANIFEST.md) for pin/override semantics.

## File env contract

| Variable | Required | Default | Notes |
|---|---|---|---|
| `MODE` | no | `SeedOnce` | One of `SeedOnce`, `Overwrite`, `AbortDeploy`, `AdoptValid`. |
| `ASSET_S3_URI` | yes | — | e.g. `s3://my-bucket/bridge.yaml`. |
| `EFS_TARGET_PATH` | yes | — | e.g. `/var/lib/gobridge/bridge.yaml`. |
| `EXPECTED_HASH` | yes for `AbortDeploy`, optional otherwise | — | Hex SHA-256 of the canonicalized asset (no `sha256:` prefix). When set in non-Abort modes, mismatch is logged at `warn` but never fails. |
| `LOG_STREAM_PREFIX` | no | — | Echoed back into every JSON log line as `stream` for grep-ability. |

## DynamoDB env contract

| Variable | Required | Notes |
|---|---|---|
| `MODE` | no | `SeedOnce` by default; also `Overwrite`, `AbortDeploy`, `AdoptValid`. |
| `TABLE` | yes | Runtime physical config table name, resolved through the ECS environment. |
| `PK` | yes | `config#<bridge_id>`; the sort key is always `current`. |
| `ITEM_S3_URI` | yes | S3 asset containing the validated bridge config JSON, not an AWS AttributeValue envelope. |
| `EXPECTED_HASH` | yes | SHA-256 of the exact asset bytes. A mismatch fails closed with exit 30. |
| `LOG_STREAM_PREFIX` | no | Optional `stream` log field. |

Synth serializes `Materialized.Config` with `parser.MarshalBridgeConfigJSON`,
preserving every typed plugin's `options` and large integers. The immutable
asset must contain resolved physical names, not CDK tokens. HA data-table names
already follow that rule. Config-table and S3 tokens stay in ECS environment
values, where CloudFormation resolves them.

### DynamoDB modes and write safety

Every read is strongly consistent. Drift hashes are computed from the actual
`data.S` JSON with sorted keys and exact numbers, ignoring only the top-level
`version`. A stored hash attribute is never trusted: admin saves replace the
row and may remove it. JSON formatting and numeric scale do not cause drift.

- **SeedOnce:** seed an absent row with `attribute_not_exists(PK)`. An existing
  valid config is never overwritten; drift logs `hash_mismatch_kept_existing`.
  If another control wins the conditional put, read and check its row.
- **Overwrite:** read the current version, then conditionally put version + 1.
  Only `ConditionalCheckFailedException` causes another read/put attempt, with
  at most three attempts. Other DynamoDB/API failures fail immediately.
- **AbortDeploy:** read-only. Missing current config or semantic drift exits 10.
- **AdoptValid:** the worker default, also read-only. Valid drift logs
  `adopted_existing_config` and succeeds, so replacement workers can boot after
  admin edits. Missing config exits 10; malformed config exits 30.

The row has `PK.S`, `SK.S`, `data.S` and `version.N`. New rows start at version 1;
the caller's YAML version never resets a stored counter. Each write sets the
JSON version to the same incremented row version so polling watchers see the
change. A missing version is legacy zero. Invalid, negative, fractional or
out-of-range versions fail closed, as does incrementing the maximum signed
64-bit version. Reads use the row version even when externally seeded JSON
carries a different version; writes always set both versions together.
The data limit is 390 KiB; each put also checks the total 400 KiB item
limit. No config content or raw API error is logged.

The current-config gate checks JSON and the bridge/list/object wire shape; the
main runtime still performs full typed-plugin and graph validation. This script
does not duplicate the Go validator. Switching sources does not migrate EFS
admin edits: choose the seed input deliberately.

IAM roles are **task-wide**, not per container. Control uses the existing config
table read/write grant; workers retain read-only access. CDK rejects worker
`SeedOnce`/`Overwrite` modes rather than granting their seeders writes. Both task
roles receive asset-read and seeder-log grants.

## File mode behavior

- **`SeedOnce`** (default) — if `EFS_TARGET_PATH` is absent, download +
  canonicalize + atomic-mv → exit `0`. If present, compare canonical hashes
  and emit `info` (match) or `warn` (mismatch); exit `0` either way. The
  Admin API is the source of truth; CDK only seeds when the file is missing.
  Runs on the **control** task (RW EFS mount).
- **`Overwrite`** — always download + canonicalize + atomic-mv → exit `0`.
  CDK / GitOps is the source of truth. Control task (RW).
- **`AbortDeploy`** — download + canonicalize the asset; if the EFS file is
  missing OR canonical hashes differ → exit `10` with `expected`/`actual`
  hashes in the log line. Strict drift gating. Read-only mount: stages the
  canonical asset under `/tmp/seeder`, only *reads* the EFS target.
- **`AdoptValid`** (default for **worker** tasks) — worker startup gate that
  **coexists with Admin-API hot reconfiguration**. A worker cannot write EFS,
  so it adopts whatever valid `bridge.yaml` the control node last wrote — the
  CDK seed *or* a later Admin-API `config-txn` commit. Behaviour:
  - target absent → exit `10` (`target_absent`): a worker with no config
    bridges nothing.
  - target unparseable → exit `30` (`yaml_unparseable`): fail closed rather
    than adopt garbage.
  - target present + hashes match the synth-time asset → exit `0`
    (`hash_match`).
  - target present + hashes **differ** → exit `0` (`adopted_existing_config`,
    logged at `warn`). This is the key difference from `AbortDeploy`: a worker
    never wedges on hash drift, so scale-out and crash-replacement keep working
    after any admin edit. Read-only mount (stages under `/tmp/seeder`).

### Reconfiguration paths (why `AdoptValid` is the worker default)

Two documented ways to change a running bridge's config must be able to
coexist:

1. **CDK redeploy** rewrites the S3 asset and, on the control task, reseeds
   `bridge.yaml` (`SeedOnce`/`Overwrite`).
2. **Admin API** `config-txn` commit rewrites the EFS `bridge.yaml` in place at
   runtime.

If workers ran `AbortDeploy`, any Admin-API edit (path 2) would make every
subsequently-launched worker (scale-out, crash replacement) fail init on the
hash mismatch versus the older synth-time asset — until the next CDK deploy.
`AdoptValid` resolves this: the control task remains the writer/gate, workers
adopt the current valid file. Set `WorkerSeederMode: "AbortDeploy"` on the
cluster construct only if you want strict lock-step and never use the Admin
API to reconfigure.

## Atomic write

The canonical asset is written to `mktemp` **inside `dirname(EFS_TARGET_PATH)`**
so the final `mv` is a same-filesystem rename (POSIX `rename(2)` is atomic).
The download itself stages under `/tmp` because S3 transfers can be large
and EFS writes are billed.

## Exit codes

| Code | Reason |
|---|---|
| `0` | Success (seeded, or canonical hashes matched, or `AdoptValid` adopted the existing valid config). |
| `10` | `AbortDeploy`/`AdoptValid` target missing, or `AbortDeploy` hashes differ. |
| `20` | S3 download failed (IAM/network). |
| `30` | YAML unparseable (asset or existing target). |
| `40` | EFS mount not writable (mkdir/mktemp probe failed). |
| `50` | Canonicalizer missing — `python3` or `PyYAML` absent (broken image). |
| `1` | Unanticipated bug or invalid env (caught by `EXIT` trap). |

For DynamoDB, exit 30 also covers invalid JSON/items, mismatched asset hashes,
version overflow and oversize payloads. Exit 60 reports DynamoDB read/write
failure or `cas_exhausted`. Exit 50 requires only a missing `python3`, not PyYAML.
Unexpected Python failures emit `internal` with exit 1. All DynamoDB paths emit
exactly one terminal JSON line, including API and validation failures.

## JSON log shape

Every terminal outcome emits exactly one JSON line on `stdout`:

```json
{"level":"error","ts":1714867200000,"mode":"AbortDeploy","reason":"hash_mismatch","exit":10,"expected":"sha256:abc","actual":"sha256:def"}
```

Required keys: `level`, `ts` (unix-ms), `mode`, `reason`, `exit`. Optional
extras (`stream`, `target`, `hash`, `expected`, `actual`, `uri`, `path`,
`var`, `source`, `value`) are emitted as flat string fields.

## Override paths

- Override pinned image: pass `SeederImage` prop to the CDK `Seeder`
  construct — overrides `image.txt` entirely.
- Refresh pinned digest:

  ```sh
  SEEDER_IMAGE='docker.io/mariotoffia/gobridge-seeder@sha256:<digest>' \
    make -C deployment/aws-filebased-config update-seeder-image
  ```

## Run locally

```sh
# Fake aws CLI, dump a fixture, run the script.
export PATH="$(pwd)/tests/fixtures/bin:$PATH"   # see tests/run.sh
export MODE=SeedOnce
export ASSET_S3_URI=s3://test/bridge.yaml
export EFS_TARGET_PATH=/tmp/efs/bridge.yaml
./seeder.sh
```

The included [tests/run.sh](tests/run.sh) runs file/image cases plus
[DynamoDB cases](tests/ddb_cases.py) using the same AWS CLI fixture, without
Docker. It requires `bash` and `python3` (PyYAML for file cases only). DynamoDB
cases check conditional puts, bounded conflicts, exact versions, read-only
drift modes, malformed items, asset integrity and API failures.
