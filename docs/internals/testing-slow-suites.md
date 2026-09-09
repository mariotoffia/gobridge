# Deployment, long-running and shell test suites

## Overview

Deployment tests check the running profile. Long-running suites cover sustained
load and recovery. Build-input checks verify how initial config enters an image.

## 5.6 Deployment tests

A deployment test deploys the shipped CDK profile and drives the running system,
so it proves what synth assertions assume. They live in
`deployment/aws-filebased-config/cdk/integration/` and are the one place build
tags are correct: they gate not "is Docker here" but cost.

| Tag | Backend | Gate |
|---|---|---|
| `integration_aws` | a real, credentialed AWS sandbox | `GOBRIDGE_INT_*`, including required `GOBRIDGE_INT_VERSION` for per-fixture Go-built images. Real account, real money. |
| `integration_local` | the same stack via `cdklocal`, on emulators | `GOBRIDGE_INT_LOCAL=1`, Docker and Node. No account, no credentials. |

One harness serves both: the sandbox, the deploy/destroy calls and the
outputs-file contract are shared, and the local backend is one branch in each.
What a deployed system must do is asserted once against a probe the two backends
supply differently, so the proofs cannot drift apart. `GOBRIDGE_INT_KEEP=1`
keeps the stack and everything it runs on.

Credentialed fixtures require `GOBRIDGE_INT_VERSION`, a published profile `lib`
version supporting embedded config and `-initial-config-digest`.
`ImageFromGoBuild` builds each fixture with its parsed config.
`GOBRIDGE_INT_IMAGE` registry overrides are rejected; compatible publication
is a prerequisite, not something a locally passing suite proves.

Local runs read the embedded payload from each staged
`initial-config-<digest>.goenv` file's `GOFLAGS`, then build the root Dockerfile
from this checkout with those exact bytes. They do not install a published
module. `GOBRIDGE_LOCAL_IMAGE` skips that build only if its
`-initial-config-digest` output matches the staged config. The probe runs with
networking disabled. Missing support or a mismatched hash fails the run; unset
the override to build the required fixture image.

**What a local run proves, and what it does not.** It proves the runtime
contract on a deployed stack, and — because the emulator runs each task
definition as a real container — that the synthesized shape wires identity
correctly. It does NOT prove AWS behaves as declared: the emulator drops
task-definition volumes, serves no task metadata, cannot carry EFS, and has no
container-dependency model, so the harness restores the first three and says so
where it does. Initial config is now created inside the control process, not
by an init container; its proof must not depend on container start ordering.
The emulator also does not evaluate IAM, never evaluates an alarm, cannot
update an `AWS::ECS::Service`, and does not route a load balancer to a task.
Any published claim must name which half it rests on.

**The matrix, and the reason for every entry that has no local test**, lives in
`docs/aws-deployment/local-deployment-suite.md`. A behaviour that cannot be
proved locally is recorded there with what was measured, not with an
assumption — and where a gap can be partly closed from the other side (the
health-check path probed against the container, the alarm's own query replayed
through `GetMetricData`, the deployed role's policy read back through IAM), it
is.

The DynamoDB-config deployment proof is
`TestLocal_DynamoDBConfigHotReload`. Run it alone, still rebuilding the runtime
image and provisioning the local tools, with:

```bash
make test-local-deploy LOCAL_DEPLOY_RUN='^TestLocal_DynamoDBConfigHotReload$'
```

The initialization proof must exercise the embedded initial document through
the shipped command, three-member generation-zero convergence, and two direct
CAS table writes followed by per-member applied-config reads. A harness-side
initial config write or sidecar would bypass the behavior under test.
Deletion/recreation tests must prove safe standalone idle and rebuild, plus
process exit for clustered deletion or uncertain teardown. Read-error cases
must instead preserve last-success degraded operation. The local storage adapter's
fast regression checks are `TestDeclaredTaskSpec_ConfigStorage` and
`TestVerifyVolumeFreeTask` under the same `integration_local` build tag; they
require neither Docker nor `GOBRIDGE_INT_LOCAL`.
`TestDynamoDBConfigFixture_IsolatesRolloutBaseline` is a non-race CDK fixture
check: the DynamoDB-config scenario's stack-scoped bridge ID keeps its rollout
mirror separate from the existing file proof and from repeated deployments.

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
- The release subset: `make test-release-gate` runs only the proofs a release is
  gated on, selected by exact test name, and then the finite-cgroup proof. The
  names live in `RELEASE_LONGRUNNING_TESTS` in the Makefile and are pinned
  against the suite by `tests/docsexamples`, because `go test -run` treats a
  pattern that matches nothing as success — a renamed proof would otherwise drop
  out of the gate while the run still reported green.
- The published soak: `make test-soak` sets `GOBRIDGE_SOAK_DURATION=60m`. The
  ordinary suite runs the same test at a 5-minute smoke profile so it stays
  usable; the hour is the interval a slow goroutine, timer, connection or memory
  leak needs to become visible. Budget over an hour and let it finish.
- Mutation fuzzing: `make fuzz` mutates each target in `FUZZ_TARGETS` for
  `FUZZTIME` (default 5m). Fuzz targets are NOT longrunning-tagged — their seed
  corpora run in `make test` on every pull request — so the CI workflow also
  carries a `workflow_dispatch` fuzz job. A crasher lands in the package's
  `testdata/fuzz` directory; commit it, and it becomes a seed.
- CI: **compiled, never run**. Nothing carrying the `longrunning` tag runs in
  the cloud — not on a PR, not on a schedule, not in a release gate. It IS
  compiled on every pull request (`go vet -tags=longrunning ./...` in the CI
  test job and again in `make lint`), because the module has no default-tag
  packages: every ordinary module walk lists nothing for it and skips it, so a
  refactor could break every production proof in it while the branch stayed
  green. `tests/docsexamples` pins that the lint target still does this. That includes the two
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

## 10. Deployment build-input checks

Configuration seeder scripts and their image-updater suites are removed.
Initial-config build checks cover the root Make target, Docker build argument,
and CDK Go-build asset. They must verify native `GOENV` file flags, decoding
of `main.initialConfigBase64`, and a document large enough to expose argument
limits. Go `@responsefile` syntax is not supported.

Synth checks must reject a separate config S3 asset, download grant, seeder
container, or worker config-write grant. Registry images must remain unchanged.
Runtime creation tests must protect present invalid and legacy documents,
assign target version 1, reread the creation winner, and preserve logical
credential references.

Snapshot tests cover mutable plugins with `ports.FreezableConfig` and deeply
immutable scalar value configs without it. Admission mutations must not change
the published config. Observation tests cover one authoritative layer, rejection
of overlays, and `Manager.NotifyIdle` preserving a newer desired snapshot.

File-initialization tests exercise local filesystems. They do not prove EFS
crash durability; that requires separate evidence on the deployed filesystem.

See [initial configuration](../aws-deployment/config-initialization.md) for the
behavior being tested and
[base image digests](../../DEVELOPMENT.md#base-image-digests) for image-pin checks.
