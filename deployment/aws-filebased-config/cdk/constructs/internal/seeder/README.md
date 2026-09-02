# Seeder

Init container that materializes (or drift-checks) `bridge.yaml` on the EFS
RW mount before the main GoBridge service container starts.

The container image is based on the upstream `public.ecr.aws/aws-cli/aws-cli`
pinned by [image.txt](image.txt), which ships `aws` and `python3` but NOT the
`PyYAML` the canonicalizer needs — [Dockerfile](Dockerfile) layers that on.
See [MANIFEST.md](MANIFEST.md) for pin/override semantics.

## Env contract

| Variable | Required | Default | Notes |
|---|---|---|---|
| `MODE` | no | `SeedOnce` | One of `SeedOnce`, `Overwrite`, `AbortDeploy`, `AdoptValid`. |
| `ASSET_S3_URI` | yes | — | e.g. `s3://my-bucket/bridge.yaml`. |
| `EFS_TARGET_PATH` | yes | — | e.g. `/var/lib/gobridge/bridge.yaml`. |
| `EXPECTED_HASH` | yes for `AbortDeploy`, optional otherwise | — | Hex SHA-256 of the canonicalized asset (no `sha256:` prefix). When set in non-Abort modes, mismatch is logged at `warn` but never fails. |
| `LOG_STREAM_PREFIX` | no | — | Echoed back into every JSON log line as `stream` for grep-ability. |

## Mode behavior

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

The included [tests/run.sh](tests/run.sh) exercises the critical paths
(`SeedOnce`, `AbortDeploy`, and all four `AdoptValid` outcomes) without
Docker, requiring only `bash`, `python3` (with PyYAML), and
`sha256sum`/`shasum`.
