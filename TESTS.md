# Test Authoring Rules

This document is **not** a usage guide — it is the contract every new or
modified test in gobridge must follow. The rules here exist to keep the
test suite **non-flaky**, **architecturally correct** (no leakage from
adapters back into the domain), and **fast enough that `make test` is a
green-or-red signal you can trust on every save**.

If a rule below conflicts with what an existing test does, the rule
wins; the existing test is wrong and should be rewritten when touched.

The architecture you are testing is described in
[DDD.md](DDD.md), [UBIQUITOUS.md](UBIQUITOUS.md), and
[ARCHITECTURE.md](ARCHITECTURE.md). Use the terms from those documents
in test names, helper names, and comments.

---

## 1. The three test categories

gobridge has **exactly three** kinds of tests. Every test must be
identifiable, on sight, as one of them. Mixing categories in a single
test function is forbidden.

| Category | Lives in | Build tag | Skip mechanism | Run with | Time budget |
|---|---|---|---|---|---|
| **Unit** | next to the code: `foo_test.go` beside `foo.go` | none | n/a (always runs) | `make test` (`-short -race -timeout 120s`) | < 100 ms / test, < 2 min total |
| **Integration** | `*_test.go` in any package, **plus** `tests/integration/...` for cross-module flows | none | `testing.Short()` + Docker probe in `testutil/*` | `make test-integration` | seconds; full target ≤ 10 min |
| **Long-running** | `tests/longrunning/` only | `//go:build longrunning` (mandatory on every file) | n/a — invisible to default `go test` | `make test-long-running` | minutes to hours; tagged because nobody wants this on PRs |

### Decision tree — which category am I writing?

```
Does the test need a Docker container, real network, or > 1 s wall clock?
├─ no  → UNIT (everything in this branch must be deterministic and < 100 ms)
└─ yes → does it intentionally exercise long-haul behaviour
         (broker crash, > 60 s back-pressure, soak, leak detection)?
         ├─ no  → INTEGRATION (gated by testing.Short and Docker probe)
         └─ yes → LONG-RUNNING (`//go:build longrunning`, tests/longrunning/)
```

A test that "feels integration-y" but uses only fakes is still a
**unit** test. A test that is fast but talks to Docker is still an
**integration** test (because removing Docker on a contributor's
laptop must not turn it red).

---

## 2. Anti-flake rules (apply to every category)

These are the rules that, when broken, produce the dreaded
"works on my machine, fails on CI" tickets. They are non-negotiable.

### 2.1 No `time.Sleep`. Ever.

`time.Sleep` is banned in production code (`make audit-timings`) **and
in test code** (`make audit-test-timings` — runs as part of
`make test`). The legitimate reasons people reach for it are all
covered by named helpers:

| You think you need… | Use instead |
|---|---|
| "wait until a goroutine has started" | a `chan struct{}` started signal, or `ports.ReceiverStartedSignaler` |
| "wait until a metric was emitted" | poll `ports.RecordingExporter.FindEntries` with `require.Eventually` |
| "wait for a state change" | `require.Eventually(t, cond, timeout, interval)` from `testify` |
| "advance time" | inject `domain/clock` and use `domain/clock/clocktest` to drive ticks deterministically |
| "let the scheduler run" | redesign — you have a real ordering bug that `runtime.Gosched()` is hiding |

If you cannot express a wait without `time.Sleep`, the production code
is not testable; fix the production code (it almost always means a
missing `Clock` dependency or a missing started-signal channel).

### 2.2 Time is injected, never read

Production code under test must take a `domain/clock.Clock`. Tests
construct a `clocktest.FakeClock`, advance it explicitly, and assert
the resulting behaviour. A test that reads `time.Now()` is a flake
waiting to happen and **will** fail under load on CI.

### 2.3 Concurrency is asserted, not assumed

- Always run with `-race` (default in every Make target).
- For background goroutines, use `t.Cleanup(cancel)` so leaks crash
  the test, not a later one. `tests/longrunning/gap_goroutine_leak_test.go`
  exists exactly because leaks routinely hide behind passing tests.
- Synchronise on channels or `sync.WaitGroup`, not on sleeps or
  hard-coded "100 ms ought to be enough".
- Do not call `t.Parallel()` in tests that share a global resource
  (`ports.DefaultRegistry`, environment variables, working directory).

### 2.4 Determinism over realism

- Random IDs in tests must come from a seeded source you control,
  or from a per-test `uuid.NewString()` whose value you do not assert
  on.
- Map iteration order is undefined; never assert "the third key is
  X". Build a sorted slice or a set comparison.
- JSON ordering is undefined unless you canonicalise. Use
  `assert.JSONEq` or compare decoded values.

### 2.5 No external network, ever

Unit and integration tests must not reach the real internet. Docker-
backed integration tests use the `testutil/*local` packages, which
bind to `127.0.0.1`. If a test fails because DNS is down on CI, the
test is wrong.

### 2.6 Cleanup is mandatory and ordered

Every resource (goroutine, file, container, channel) is registered
with `t.Cleanup` or `defer` **at the moment it is acquired**, not
"later, at the end of the test". This guarantees cleanup runs even
when an early `require` aborts.

---

## 3. Architectural correctness

Tests are part of the architecture. They must respect the same
inward-pointing dependency rule as production code (see
[DDD.md § 4](DDD.md) and `.go-arch-lint.yml`).

### 3.1 Dependency direction

| Test for code in… | May import |
|---|---|
| `domain/<context>/...` | only stdlib + sibling files in the same context + `domain/clock/clocktest` (excluded from arch lint) |
| `ports/...` | `domain/*`, `ports`, `testify`, `ports/storetest` |
| `runtime/`, `bridge/`, `validate/`, `circuitbreaker/`, `config/` | their own production deps + fakes defined inside the same package's `fakes_test.go` |
| an adapter (`adapters/<vendor>/<role>/<tech>/...`) | its own production deps + the matching `testutil/*local` helper + `ports/storetest` (for store adapters) |
| `tests/integration/` | the public API surface only — **never** unexported types from any package |
| `tests/longrunning/` | the public API surface only |

A test file that imports a sibling adapter (e.g. an MQTT test that
imports the SQS adapter) is an architectural bug and will fail
`make lint-arch` once the offending edge becomes a non-test import —
do not anticipate it by adding the edge "for the test only".

### 3.2 No domain leakage through fakes

Fakes that satisfy a port interface must live in `fakes_test.go`
inside the package whose code consumes the port. They are
hand-rolled — **gomock and mockery are banned** because their
generated code re-asserts argument equality in ways that cannot
distinguish "logically equal envelope" from "same pointer".

A fake must:

- implement only the port interface it claims to implement;
- never import another adapter;
- never embed production state from `runtime`/`bridge`;
- expose its observable state through plain fields or accessor
  methods (`Acked() bool`, `Sent() []ports.OutboundMessage`).

The canonical reference is `runtime/fakes_test.go`.

### 3.3 Conformance suites are the contract

Every store implementation **must** call the matching conformance
suite from `ports/storetest`:

```go
func TestMyDLQStore(t *testing.T) {
    storetest.RunDLQStoreTests(t, newStore(t))
}
```

If the conformance suite does not test what you need to test, the
suite is incomplete — extend it (in `ports/storetest`) so every
implementation gets the new check, rather than writing a one-off
test in your adapter package.

### 3.4 Subject vs Address

When asserting outbound traffic, assert against
`ports.OutboundMessage{Envelope, Address}` — **never** assume the
runtime wrote the address into `Envelope.Subject`. The split
(`Subject` = logical, `Address` = transport destination) is an
invariant; tests that conflate the two will mask real bugs.

### 3.5 Reserved headers

Tests that exercise ingress paths must verify reserved-prefix
(`x-bridge.*`) headers are stripped. Tests that exercise egress
paths must verify the runtime's reserved headers (correlation ID,
trace context, route ID, source ID) are present.

---

## 4. Unit tests

### 4.1 Naming and structure

- One test function per behaviour: `TestEnvelope_IsExpired_True`,
  `TestEnvelope_IsExpired_False`, **not** `TestEnvelope`.
- Use `t.Run(...)` only for table-driven cases of the same behaviour.
- File: `foo_test.go` next to `foo.go`. Black-box (`package foo_test`)
  unless you must touch unexported identifiers.

### 4.2 Assertions

- `stretchr/testify/{assert,require}` is the default in the root
  module. Use `require` for preconditions (test cannot continue),
  `assert` for the actual claims.
- For errors, `errors.Is` / `errors.As` only — never compare
  `err.Error()` strings (they are not part of the contract).
- For `domain.BridgeError`, assert on `Code` and `Class`, never on
  the formatted message.

### 4.3 Determinism

Unit tests that touch any of {time, randomness, goroutines, the
file system} must inject the dependency. The package under test
should have a constructor that accepts a `Clock`, `Reader`, etc.
If it does not, fix the constructor before fixing the test.

### 4.4 Forbidden in unit tests

- Network calls of any kind.
- Docker.
- `time.Sleep`.
- Reading `time.Now()`.
- `os.Setenv` without `t.Cleanup` to restore it.
- Touching `ports.DefaultRegistry` without first
  `t.Cleanup(func() { ports.DefaultRegistry = oldRegistry })` or using
  `ports.NewRegistry()` for an isolated instance.

---

## 5. Integration tests

Integration tests verify that an adapter conforms to its port
contract against a **real** dependency (a Docker-managed broker,
DynamoDB Local, ElasticMQ, the ASB emulator, MinIO).

### 5.1 Skip discipline — the contract that keeps `make test` green without Docker

Every integration test must be skipped when the local environment
cannot run it. The `testutil/*local` helpers do this for you:

```go
func TestMain(m *testing.M) {
    ddblocal.Configure(ddblocal.WithCleanOrphans(true))
    code := m.Run()
    ddblocal.Shutdown()
    os.Exit(code)
}

func TestDynamoDBOutbox_Persist(t *testing.T) {
    client := ddblocal.Client(t) // skips automatically when:
                                  //   - testing.Short() is true, or
                                  //   - Docker is unavailable and no
                                  //     DYNAMODB_ENDPOINT env var is set
    // ... use client ...
}
```

Do **not** invent your own skip logic. If a new dependency needs a
container, add a `testutil/<thing>local` package modelled on the
existing ones; do not embed `docker run` calls in tests.

### 5.2 No build tags

Integration tests are gated by `testing.Short()` and the Docker
probe — **not** by a build tag. A `//go:build integration` line in
an integration test is wrong; remove it.

### 5.3 Container hygiene

- Containers are named `gobridge-<package>-<uuid>`. Never use a
  fixed name; parallel CI jobs will collide.
- `WithCleanOrphans(true)` in `TestMain` removes leftovers from
  earlier runs — this is the only cleanup mechanism the suite relies
  on, so container names must keep the `gobridge-` prefix.
- If you need to nuke leftovers manually, `docker ps -aq --filter name=gobridge-`
  followed by `docker rm -f` does it; there is no Make target for
  this on purpose (the helpers self-clean).

### 5.4 What to assert

Integration tests assert on the **port contract**, not on the
vendor SDK shape. If your test ends up reading
`*sqs.ReceiveMessageOutput.Messages[0].MessageId`, that is unit-test
territory — replace it with an assertion through the
`ports.Receiver` / `ports.Delivery` surface.

### 5.5 Cross-module flows belong in `tests/integration/`

A test that wires the full bridge (config → builder → runtime →
Docker brokers → assertions) is **not** an adapter test; it is an
end-to-end test and lives in `tests/integration/`. These tests
import only the public API of each module.

---

## 6. Long-running tests

These exist to catch what unit and integration tests cannot:
goroutine leaks, soak behaviour, broker-crash recovery, real
back-pressure, multi-hop flows, lease takeover races. They are
expensive and must remain invisible to default `go test` runs.

### 6.1 Mandatory shape

Every long-running test file starts with:

```go
//go:build longrunning

package longrunning
```

The build tag is the only thing keeping these tests off PR runs.
A long-running test without the tag is a CI accident waiting to
happen.

### 6.2 Where they live

- Directory: `tests/longrunning/` (its own Go module — see
  `tests/longrunning/go.mod`).
- One file per use-case or gap: `uc<NN>_<topic>_test.go` for
  scenario tests, `gap_<topic>_test.go` for gap probes
  (e.g. `gap_goroutine_leak_test.go`).
- Shared helpers in `longrunning_test.go` and
  `longrunning_perf_helpers_test.go`.

### 6.3 How they run

- Locally: `make test-long-running` (10 800 s timeout, requires
  Docker, writes `reports/test-long-running.log`).
- CI: scheduled (nightly / pre-release), **not** on every PR.
- They never run inside `make test` or `make test-integration` —
  the Makefile excludes `tests/longrunning/` explicitly.

### 6.4 Determinism even at length

Long-running ≠ allowed-to-be-flaky. The same anti-flake rules
apply:

- No `time.Sleep` for synchronisation. Use `clocktest`, channels,
  or `require.Eventually` with a generous timeout.
- Fail loudly: a leak detector that prints a warning and passes
  is worse than no leak detector. `t.Fatalf` on the first proven
  leak.
- Bound resource usage. A soak test that allocates without bound
  cannot tell flakiness from a real regression.

### 6.5 What belongs here

| Belongs in long-running | Belongs in integration | Belongs in unit |
|---|---|---|
| broker crash + reconnect | adapter sends a message and gets an ack | the BackoffPolicy multiplier is correctly applied |
| 60-minute soak with leak detection | round-tripping a message through a real container | `Envelope.Clone()` deep-copies headers |
| multi-hop bridge mesh | a single bridge instance with one route | route policy normalisation |
| lease handover under load | one acquire + one renew | `LeaseToken.Version` monotonicity |

If it can be shrunk to seconds without losing meaning, it is an
integration test, not a long-running one.

---

## 7. Test fixtures and shared helpers

- `testutil/*local` — Docker container helpers (DynamoDB, SQS, ASB,
  S3). Each exposes `Configure`, `Shutdown`, and a typed
  `Client(t)`.
- `testutil/tlsgen` — pure-crypto TLS material generator. No Docker.
- `domain/clock/clocktest` — fake clock. The only blessed way to
  drive time forward.
- `ports/storetest` — conformance suites for `LeaseStore`,
  `OutboxStore`, `DLQStore`. Mandatory for every new store.
- `ports.RecordingExporter` — records metric emissions for
  assertion. Use `FindEntries(name)` rather than indexing into the
  internal slice.

When the helper you need does not exist, add it next to the existing
ones using the same shape. Do not inline its logic into the test.

---

## 8. Running the suite

```bash
# Unit tests + timing audits. Must pass on every save.
make test

# Unit + integration. Requires Docker. Used by `make check-all`.
make test-integration

# Long-running suite (build tag `longrunning`, Docker required, hours).
make test-long-running

# CI gates
make check       # build + lint + unit
make check-all   # build + lint + integration
```

`make test` is the contract: it must remain green on a fresh laptop
with no Docker. If a change to a unit test breaks that contract,
the change is wrong.

---

## 9. Checklist for every new test

Before you push, walk this list explicitly. Yes, it is tedious; that
is the point.

- [ ] The test is unambiguously **unit**, **integration**, or
      **long-running** — and is in the right directory and (for
      long-running) carries `//go:build longrunning`.
- [ ] No `time.Sleep`. No `time.Now()` outside an injected `Clock`.
- [ ] All goroutines, files, containers, env vars, and registry
      mutations are registered with `t.Cleanup` at acquisition.
- [ ] Assertions go through ports, not vendor SDK types.
- [ ] Errors compared with `errors.Is` / `errors.As`, not strings.
- [ ] If the code under test is a store, the conformance suite
      from `ports/storetest` is invoked.
- [ ] The test runs green under `-race`.
- [ ] No imports of sibling adapters, sibling processors, or
      unexported identifiers from another module.
- [ ] If it needs Docker, it skips automatically when Docker is
      absent (uses a `testutil/*local` helper).
- [ ] If it is long-running, the build tag is on every file in the
      package, and the test fails fast on a real regression rather
      than logging and continuing.

A reviewer will check the same list. Saving them the round-trip is
the whole point of having it written down.
