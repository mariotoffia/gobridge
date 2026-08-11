# Test Authoring Rules

Contract every new or modified test in gobridge must follow. Goal:
non-flaky, architecturally correct, fast enough that `make test` is a
trustworthy green-or-red signal on every save.

If a rule conflicts with an existing test, the rule wins. Rewrite the
existing test when you touch it.

Architecture under test: [DDD.md](DDD.md), [UBIQUITOUS.md](UBIQUITOUS.md),
[ARCHITECTURE.md](ARCHITECTURE.md). Use those terms in test names,
helper names, and comments.

---

## 1. Three categories

Exactly three. Every test is identifiable on sight as one. Mixing
categories in a single test function is forbidden.

| Category | Lives in | Build tag | Skip mechanism | Run with | Time budget |
|---|---|---|---|---|---|
| **Unit** | `foo_test.go` next to `foo.go` | none | n/a | `make test` (`-short -race -timeout 120s`) | < 100 ms / test, < 2 min total |
| **Integration** | `*_test.go` in any package + `tests/integration/` for cross-module flows | none | `testing.Short()` + Docker probe in `testutil/*` | `make test-integration` | seconds; full target ≤ 10 min |
| **Long-running** | `tests/longrunning/` only | `//go:build longrunning` (mandatory on every file) | n/a — invisible to default `go test` | `make test-long-running` | minutes to hours |

### Decision tree

```
Needs Docker, real network, or > 1 s wall clock?
├─ no  → UNIT (deterministic, < 100 ms)
└─ yes → exercises long-haul behaviour (broker crash, > 60 s back-pressure, soak, leak detection)?
         ├─ no  → INTEGRATION (testing.Short + Docker probe)
         └─ yes → LONG-RUNNING (//go:build longrunning, tests/longrunning/)
```

A test that "feels integration-y" but uses only fakes is **unit**. A
fast test that talks to Docker is **integration** (removing Docker on
a contributor's laptop must not turn it red).

---

## 2. Anti-flake rules (every category)

### 2.1 No `time.Sleep`. Ever.

Banned in production (`make audit-timings`) and tests
(`make audit-test-timings`, runs as part of `make test`).

| You think you need… | Use instead |
|---|---|
| "wait until a goroutine has started" | `chan struct{}` started signal, or `ports.ReceiverStartedSignaler` |
| "wait until a metric was emitted" | poll `ports.RecordingExporter.FindEntries` with `wait.Until` |
| "wait for a state change" | `testutil/wait.Until(t, timeout, desc, cond)` (backoff + `t.Deadline()` clamp; `require.Eventually` acceptable) |
| "dump diagnostics before failing a wait" | `testutil/wait.Poll(timeout, cond)` — non-failing; then log and `t.Fatalf` yourself |
| "wait for a container to be up / stopped / gone / log a line" | `testutil/dockerexec.WaitHealthy / WaitStopped / WaitGone / WaitLogLine / WaitTCP / WaitProbe` |
| "advance time" | inject `domain/clock`, drive ticks via `domain/clock/clocktest` |
| "let the scheduler run" | redesign — you have a real ordering bug `runtime.Gosched()` is hiding |

Do NOT hand-roll a sleep-interval poller (`for { if cond() ...; time.Sleep(x) }`)
— that is `wait.Until` re-implemented without the deadline clamp. The two
sanctioned homes for poll pacing are `testutil/wait` and `testutil/dockerexec`;
`make audit-test-timings` scans every other test file AND every non-test file
under `testutil/`, `ports/storetest`, and `tests/testutil` for `time.Sleep`.

If the wait cannot be expressed without `time.Sleep`, the production
code is not testable. Add a `Clock` dependency or a started-signal
channel.

### 2.2 Time is injected, never read

Production code under test takes `domain/clock.Clock`. Tests use
`clocktest.FakeClock`, advance it explicitly, assert the result.
`time.Now()` in a test is a flake under CI load.

### 2.3 Concurrency is asserted, not assumed

- Always `-race` (default in every Make target).
- Background goroutines: `t.Cleanup(cancel)` so leaks fail their own
  test, not a later one. See `tests/longrunning/gap_goroutine_leak_test.go`.
- Synchronise on channels or `sync.WaitGroup`. Never on sleeps or
  "100 ms ought to be enough".
- Do not call `t.Parallel()` in tests sharing global state
  (`*ports.Registry`, env vars, working directory).

### 2.4 Determinism over realism

- Random IDs from a seeded source you control, or `uuid.NewString()`
  whose value you do not assert on.
- Map iteration order is undefined. Never assert "third key is X".
  Build a sorted slice or set.
- JSON ordering is undefined unless canonicalised. Use
  `assert.JSONEq` or compare decoded values.

### 2.5 No external network

Unit and integration tests must not reach the real internet.
Docker-backed tests use `testutil/*local`, which bind to `127.0.0.1`.
A test that fails because DNS is down is wrong.

### 2.6 Cleanup is mandatory and ordered

Every resource (goroutine, file, container, channel) is registered
with `t.Cleanup` or `defer` **at acquisition**, not "later, at the
end". Guarantees cleanup runs even when an early `require` aborts.

### 2.7 Never call `jsii.Close()` in a CDK test

The exception to 2.6, and it is not optional. `jsii.Close()` crashes
the test binary at random.

`jsii-runtime-go`'s `ensureStarted` spawns a goroutine that sits in
`p.cmd.Wait()` on the node kernel process. `Process.Close()` writes
`{"exit":0}` to that process's stdin — deliberately making it exit —
and then calls `p.cmd.Wait()` **itself**. Two concurrent
`(*exec.Cmd).Wait` on one command. Go 1.26's `Wait` ends with
`closeDescriptors(c.parentIOPipes); c.parentIOPipes = nil`, so the
loser of the race iterates a half-torn slice and segfaults on a nil
`io.Closer`. Under Go ≤1.25 the second `Wait` merely returned
`ECHILD` ("Runtime process exited abnormally: wait: no child
processes"), which is why this only surfaced recently.

So **every `jsii.Close()` is a coin flip**, and the code is identical
in every `jsii-runtime-go` release from v1.127.0 to v1.139.0 — there
is no version to upgrade to. Let the test binary exit instead: the
kernel child gets EOF on stdin and exits on its own (verified: no
stray node processes). Removing the calls also cut the CDK module's
suite from ~110s to ~24s, because each `Close()` forced the next test
to re-spawn node and re-import the whole CDK assembly.

Do not "fix" a leak that is not there by adding it back.

---

## 3. Architectural correctness

Tests follow the same inward-pointing dependency rule as production
code (see [DDD.md § 4](DDD.md) and `.go-arch-lint.yml`).

### 3.1 Dependency direction

| Test for code in… | May import |
|---|---|
| `domain/<context>/...` | stdlib + sibling files in same context + `domain/clock/clocktest` (excluded from arch lint) |
| `ports/...` | `domain/*`, `ports`, `testify`, `ports/storetest` |
| `runtime/`, `bridge/`, `validate/`, `circuitbreaker/`, `config/` | own production deps + fakes in same package's `fakes_test.go` |
| an adapter (`adapters/<vendor>/<role>/<tech>/...`) | own production deps + matching `testutil/*local` helper + `ports/storetest` (for store adapters) |
| `tests/integration/` | public API surface only — never unexported types |
| `tests/longrunning/` | public API surface only |

A test importing a sibling adapter is an architectural bug. Do not
add the edge "for the test only".

### 3.2 No domain leakage through fakes

Fakes satisfying a port live in `fakes_test.go` inside the consumer
package. Hand-rolled only — **gomock and mockery are banned**
(generated argument-equality cannot distinguish "logically equal
envelope" from "same pointer").

A fake must:

- implement only the port interface it claims;
- never import another adapter;
- never embed production state from `runtime`/`bridge`;
- expose observable state via plain fields or accessors
  (`Acked() bool`, `Sent() []ports.OutboundMessage`).

Reference: `runtime/fakes_test.go`.

### 3.3 Conformance suites are the contract

Every store implementation **must** call the matching suite from
`ports/storetest`:

```go
func TestMyDLQStore(t *testing.T) {
    storetest.RunDLQStoreTests(t, newStore(t))
}
```

If the suite does not test what you need, extend the suite — do not
write a one-off in your adapter package. Every implementation gets
the new check that way.

### 3.4 Subject vs Address

Assert against `ports.OutboundMessage{Envelope, Address}`. Never
assume the runtime wrote the address into `Envelope.Subject`. The
split (`Subject` = logical, `Address` = transport destination) is an
invariant; conflating them masks bugs.

### 3.5 Reserved headers

- Ingress tests: verify reserved-prefix (`x-bridge.*`) headers are stripped.
- Egress tests: verify runtime-injected headers are present
  (correlation ID, trace context, route ID, source ID).

---

## 4. Unit tests

### 4.1 Naming and structure

- One test function per behaviour: `TestEnvelope_IsExpired_True`,
  `TestEnvelope_IsExpired_False`. Not `TestEnvelope`.
- `t.Run(...)` only for table-driven cases of the same behaviour.
- File `foo_test.go` next to `foo.go`. Black-box (`package foo_test`)
  unless you must touch unexported identifiers.

### 4.2 Assertions

- `stretchr/testify/{assert,require}` is the default. `require` for
  preconditions; `assert` for actual claims.
- Errors: `errors.Is` / `errors.As`. Never compare `err.Error()`
  strings (not part of the contract).
- `domain.BridgeError`: assert on `Code` and `Class`. Never on the
  formatted message.

### 4.3 Determinism

Unit tests touching {time, randomness, goroutines, file system} must
inject the dependency. The constructor under test should accept
`Clock`, `Reader`, etc. If it does not, fix the constructor first.

### 4.4 Forbidden in unit tests

- Network calls.
- Docker.
- `time.Sleep`.
- Reading `time.Now()`.
- `os.Setenv` without `t.Cleanup` to restore.
- Touching `*ports.Registry` without `t.Cleanup` to restore the
  old registry, or using `ports.NewRegistry()` for an isolated instance.

---

## 5. Integration tests

Verify an adapter conforms to its port contract against a **real**
dependency (Docker-managed broker, DynamoDB Local, ElasticMQ, ASB
emulator, MinIO).

### 5.1 Skip discipline

Every integration test must skip when the local environment cannot
run it. The `testutil/*local` helpers do this:

```go
func TestMain(m *testing.M) {
    ddblocal.Configure(ddblocal.WithCleanOrphans(true))
    code := m.Run()
    ddblocal.Shutdown()
    os.Exit(code)
}

func TestDynamoDBOutbox_Persist(t *testing.T) {
    client := ddblocal.Client(t) // skips when:
                                  //   - testing.Short() is true, or
                                  //   - Docker is unavailable and no
                                  //     DYNAMODB_ENDPOINT env var is set
    // ... use client ...
}
```

Do not invent skip logic. If a new dependency needs a container, add
a `testutil/<thing>local` package modelled on the existing ones. Do
not embed `docker run` calls in tests.

### 5.2 No build tags

Integration tests are gated by `testing.Short()` + Docker probe, not
a build tag. A `//go:build integration` line is wrong; remove it.

### 5.3 Container hygiene

- Containers named `gobridge-<package>-<uuid>`. Never use a fixed
  name; parallel CI jobs collide.
- `WithCleanOrphans(true)` in `TestMain` removes leftovers. The only
  cleanup mechanism the suite relies on, so the `gobridge-` prefix
  is mandatory.
- Manual nuke: `docker ps -aq --filter name=gobridge-` then
  `docker rm -f`. No Make target on purpose (helpers self-clean).

### 5.4 What to assert

Assert on the **port contract**, not vendor SDK shape. If the test
reads `*sqs.ReceiveMessageOutput.Messages[0].MessageId`, it is unit
territory — replace with assertion through `ports.Receiver` /
`ports.Delivery`.

### 5.5 Cross-module flows belong in `tests/integration/`

A test wiring the full bridge (config → builder → runtime → Docker
brokers → assertions) is end-to-end, not adapter-level. Lives in
`tests/integration/` and imports only the public API of each module.

---

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
  - `TestMQTTIngressMemory` is re-run by `make test-long-running` inside a
    container with an enforced 512 MiB cgroup. It cannot assert anything
    without a real memory bound — run through the ordinary suite it detects no
    limit and skips itself — so the harness is the test, not a convenience.
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

## 7. Fixtures and shared helpers

- `testutil/*local` — Docker container helpers (DynamoDB, SQS, ASB,
  S3, MQTT, RabbitMQ, Artemis, LocalStack). Each exposes `Configure`,
  `Shutdown`, typed `Client(t)`. Readiness is a three-stage deterministic
  gate (container running → protocol-truth probe → stabilize); teardown is
  observable state (`docker` reports stopped/gone), never a sleep.
- `testutil/dockerexec` — bounded docker CLI wrapper plus the shared
  container gates (`WaitHealthy`, `WaitStopped`, `WaitGone`, `WaitLogLine`,
  `WaitTCP`, `WaitProbe`, `Stabilize`, `RemoveOrphans`, `FreePort`).
  Container chaos (kill/restart) goes through these or
  `mqttlocal.BrokerInstance` — never raw `exec.Command("docker", ...)`.
- `testutil/wait` — the canonical condition-wait (`Until`, `Poll`,
  `RequireReceive`, `RequireClosed`, `Silent`, `StableFor`).
- `tests/longrunning/nodeprocess_harness_test.go` — `nodeProcess`, the one
  separate-OS-process bridge launcher (re-exec + stdout token barriers +
  real SIGKILL). Any new multi-process scenario uses it.
- `testutil/tlsgen` — pure-crypto TLS material generator. No Docker.
- `domain/clock/clocktest` — fake clock. Only blessed way to drive
  time forward.
- `ports/storetest` — conformance suites for `LeaseStore`,
  `OutboxStore`, `DLQStore`. Mandatory for every new store.
- `ports.RecordingExporter` — records metric emissions. Use
  `FindEntries(name)`; do not index the internal slice.

When a helper does not exist, add it next to the existing ones using
the same shape. Do not inline its logic.

---

## 8. Running the suite

```bash
# Uncached unit tests + timing audits. Must pass on every save.
make test

# Uncached unit + integration. Requires Docker. Mandatory in CI and used by
# `make check-all`.
make test-integration

# Long-running suite (build tag `longrunning`, Docker required, hours).
make test-long-running

# CI gates
make check       # build + lint + unit
make check-all   # build + lint + integration
```

Every test target passes `-count=1`, writes its report under `reports/`, and
returns the failing `go test` status after report generation.

`make test` is the contract: must remain green on a fresh laptop with
no Docker. A unit-test change that breaks that contract is wrong.

---

## 9. Per-test checklist

Walk this before pushing:

- [ ] Unambiguously **unit**, **integration**, or **long-running** —
      in the right directory, and (for long-running) carries
      `//go:build longrunning`.
- [ ] No `time.Sleep`. No `time.Now()` outside an injected `Clock`.
- [ ] All goroutines, files, containers, env vars, and registry
      mutations registered with `t.Cleanup` at acquisition.
- [ ] Assertions go through ports, not vendor SDK types.
- [ ] Errors compared with `errors.Is` / `errors.As`, not strings.
- [ ] If the code under test is a store, the conformance suite from
      `ports/storetest` is invoked.
- [ ] Runs green under `-race`.
- [ ] No imports of sibling adapters, sibling processors, or
      unexported identifiers from another module.
- [ ] If it needs Docker, skips automatically when Docker is absent
      (uses a `testutil/*local` helper).
- [ ] If long-running, the build tag is on every file in the package,
      and the test fails fast on a real regression rather than
      logging and continuing.

A reviewer checks the same list.

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
workflow the root `Dockerfile` follows is in [DEVELOPMENT.md](DEVELOPMENT.md)
(Base image digests).
