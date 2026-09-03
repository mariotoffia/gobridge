# Deployment, long-running and shell test suites

## 5.6 Deployment tests

A deployment test deploys the shipped CDK profile and drives the running system,
so it proves what synth assertions assume. They live in
`deployment/aws-filebased-config/cdk/integration/` and are the one place build
tags are correct: they gate not "is Docker here" but cost.

| Tag | Backend | Gate |
|---|---|---|
| `integration_aws` | a real, credentialed AWS sandbox | `GOBRIDGE_INT_*`. Real account, real money. |
| `integration_local` | the same stack via `cdklocal`, on emulators | `GOBRIDGE_INT_LOCAL=1`, Docker and Node. No account, no credentials. |

One harness serves both: the sandbox, the deploy/destroy calls and the
outputs-file contract are shared, and the local backend is one branch in each.
What a deployed system must do is asserted once against a probe the two backends
supply differently, so the proofs cannot drift apart. `GOBRIDGE_INT_KEEP=1`
keeps the stack and everything it runs on.

**What a local run proves, and what it does not.** It proves the runtime
contract on a deployed stack, and — because the emulator runs each task
definition as a real container — that the synthesized shape wires identity
correctly. It does NOT prove AWS behaves as declared: the emulator drops
task-definition volumes, serves no task metadata, cannot carry EFS, and has no
container-dependency model, so the harness restores the first three and says so
where it does. The fourth cannot be, so a local member may start before its
init container has run — the deployment still settles, but no claim may rest on
start ordering. It also does not evaluate IAM, never evaluates an alarm, cannot
update an `AWS::ECS::Service`, and does not route a load balancer to a task.
Any published claim must name which half it rests on.

**The matrix, and the reason for every entry that has no local test**, lives in
`docs/aws-deployment/local-deployment-suite.md`. A behaviour that cannot be
proved locally is recorded there with what was measured, not with an
assumption — and where a gap can be partly closed from the other side (the
health-check path probed against the container, the alarm's own query replayed
through `GetMetricData`, the deployed role's policy read back through IAM), it
is.

## 6. Long-running tests

Catch what unit/integration cannot: goroutine leaks, soak behaviour,
broker-crash recovery, real back-pressure, multi-hop flows, lease
takeover races. Expensive; must remain invisible to default `go test`.

### 6.1 Mandatory shape

Every file starts with:

```go
//go:build longrunning

package longrunning
```

The build tag is the only thing keeping these off PR runs. A
long-running test without the tag is a CI accident.

### 6.2 Where they live

- Directory `tests/longrunning/` (own Go module — see `tests/longrunning/go.mod`).
- One file per use-case or gap: `uc<NN>_<topic>_test.go` for
  scenarios; `gap_<topic>_test.go` for gap probes
  (e.g. `gap_goroutine_leak_test.go`).
- Shared helpers in `longrunning_test.go` and
  `longrunning_perf_helpers_test.go`.

### 6.3 How they run

- Locally: `make test-long-running` (uncached, 10 800 s timeout, requires
  Docker, writes `reports/test-long-running.log`).
- CI: **never**. Nothing carrying the `longrunning` tag runs in the cloud —
  not on a PR, not on a schedule, not in a release gate. That includes the two
  bounded single-test proofs, which are developer-machine runs like the rest:
  Both bounded proofs are part of that single target, not separate ones:
  - `TestUC3SeparateProcessFailover` runs two real bridge processes against a
    real broker and DynamoDB, kills the lease owner, and asserts the standby
    recovers. It is picked up by the suite like any other test.
  - `TestMQTTIngressMemory` and `TestMQTTIngressMemoryPropertyFlood` are
    re-run by `make test-long-running` inside a container with an enforced
    512 MiB cgroup. They cannot assert anything without a real memory bound —
    run through the ordinary suite they detect no limit and skip themselves —
    so the harness is the test, not a convenience.
    `GOBRIDGE_REQUIRE_MEMORY_LIMIT=1` makes an absent limit fail instead of
    skip; Darwin retains the explicit skip.

  Run both before merging anything that touches clustering, leases, outbox
  draining or MQTT ingress: CI cannot catch a regression in them.
- Never run inside `make test` or `make test-integration` — Makefile
  excludes `tests/longrunning/` explicitly.

### 6.4 Determinism even at length

Long-running ≠ allowed-to-be-flaky. Same anti-flake rules apply:

- No `time.Sleep` for synchronisation. Use `clocktest`, channels, or
  `require.Eventually` with a generous timeout.
- Fail loudly. A leak detector that prints a warning and passes is
  worse than no detector. `t.Fatalf` on the first proven leak.
- Bound resource usage. A soak test allocating without bound cannot
  distinguish flakiness from regression.

### 6.5 What belongs here

| Long-running | Integration | Unit |
|---|---|---|
| broker crash + reconnect | adapter sends a message and gets an ack | `BackoffPolicy` multiplier correctly applied |
| 60-minute soak + leak detection | round-tripping a message through a real container | `Envelope.Clone()` deep-copies headers |
| multi-hop bridge mesh | single bridge instance with one route | route policy normalisation |
| lease handover under load | one acquire + one renew | `LeaseToken.Version` monotonicity |

Shrinkable to seconds without losing meaning → integration, not
long-running.

---

## 10. Deployment shell tests

The file-based AWS deployment ships pure-bash tests that need no Go, no Docker,
and no network:

```bash
make -C deployment/aws-filebased-config test
```

They cover two scripts:

- `seeder.sh` — the EFS config-seeder contract (single-line JSON outcome, hash
  match/mismatch, adopt/abort modes). A PATH shim mocks the `aws` CLI over
  fixture files.
- `scripts/update-image.sh` — the base-image digest refresh, exercised on BOTH
  resolver paths (crane and docker buildx) with fake `crane`, `docker`, and
  `curl` tools that model real command output and exit codes. The checks assert
  each resolver receives the exact concrete `2.x.y` reference, the manifest JSON
  reaches the verifier on stdin (the pinned digest equals the hash of the exact
  bytes), the script pins a top-level multi-platform index (OCI index or Docker
  manifest list), **verifies `linux/amd64` + `linux/arm64` before it writes**,
  prints only the pinned `image@sha256` reference, and **fails closed** (rewrites
  nothing) on a missing platform, a single-arch manifest, malformed JSON, or a
  registry that advertises no concrete `2.x.y` tag (so the mutable `2` tag is
  never pinned). Staged fail-closed cases prove that when the digest resolves but
  the Dockerfile target is bad — zero matching `FROM`, multiple matching `FROM`, a
  missing target directory, or a read-only Dockerfile — the script exits non-zero
  and **leaves both `image.txt` and the Dockerfile checksums unchanged**. A forced
  `UPDATE_IMAGE_TOOL` other than `crane`/`docker` is rejected (exit 2). The script
  never installs a tool (versions tested for this workflow: crane v0.21.7 / docker
  buildx v0.34.1).

These are the verification gates for the container-input pins. The resolve/verify
workflow the root `Dockerfile` follows is in [DEVELOPMENT.md](../../DEVELOPMENT.md)
(Base image digests).
