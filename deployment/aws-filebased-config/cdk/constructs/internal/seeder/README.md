# Seeder

Init container that materializes (or drift-checks) `bridge.yaml` on the EFS
RW mount before the main GoBridge service container starts.

The container image is the upstream `public.ecr.aws/aws-cli/aws-cli` pinned
by [image.txt](image.txt) — that image already ships `aws`, `python3`, and
`PyYAML`. See [MANIFEST.md](MANIFEST.md) for pin/override semantics.

## Env contract

| Variable | Required | Default | Notes |
|---|---|---|---|
| `MODE` | no | `SeedOnce` | One of `SeedOnce`, `Overwrite`, `AbortDeploy`. |
| `ASSET_S3_URI` | yes | — | e.g. `s3://my-bucket/bridge.yaml`. |
| `EFS_TARGET_PATH` | yes | — | e.g. `/mnt/gobridge/bridge.yaml`. |
| `EXPECTED_HASH` | yes for `AbortDeploy`, optional otherwise | — | Hex SHA-256 of the canonicalized asset (no `sha256:` prefix). When set in non-Abort modes, mismatch is logged at `warn` but never fails. |
| `LOG_STREAM_PREFIX` | no | — | Echoed back into every JSON log line as `stream` for grep-ability. |

## Mode behavior

- **`SeedOnce`** (default) — if `EFS_TARGET_PATH` is absent, download +
  canonicalize + atomic-mv → exit `0`. If present, compare canonical hashes
  and emit `info` (match) or `warn` (mismatch); exit `0` either way. The
  Admin API is the source of truth; CDK only seeds when the file is missing.
- **`Overwrite`** — always download + canonicalize + atomic-mv → exit `0`.
  CDK / GitOps is the source of truth.
- **`AbortDeploy`** — download + canonicalize the asset; if the EFS file is
  missing OR canonical hashes differ → exit `10` with `expected`/`actual`
  hashes in the log line. Strict drift gating.

## Atomic write

The canonical asset is written to `mktemp` **inside `dirname(EFS_TARGET_PATH)`**
so the final `mv` is a same-filesystem rename (POSIX `rename(2)` is atomic).
The download itself stages under `/tmp` because S3 transfers can be large
and EFS writes are billed.

## Exit codes

| Code | Reason |
|---|---|
| `0` | Success (seeded, or canonical hashes matched in `SeedOnce`/`AbortDeploy`). |
| `10` | `AbortDeploy` mode and target missing OR hashes differ. |
| `20` | S3 download failed (IAM/network). |
| `30` | YAML unparseable (asset or existing target). |
| `40` | EFS mount not writable (mkdir/mktemp probe failed). |
| `50` | Canonicalizer missing — `python3` or `PyYAML` absent (broken image). |
| `1` | Unanticipated bug or invalid env (caught by `EXIT` trap). |

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
  construct (T10) — overrides `image.txt` entirely.
- Refresh pinned digest:

  ```sh
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

The included [tests/run.sh](tests/run.sh) exercises four critical paths
without Docker, requiring only `bash`, `python3` (with PyYAML), and
`sha256sum`/`shasum`.
