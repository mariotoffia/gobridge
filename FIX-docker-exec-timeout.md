# FIX — Bound `docker run` exec time in `testutil/*local` helpers

> Companion files: `FIX-directhold-retry-pacing.md`,
> `FIX-shared-outbox-completion.md`,
> `FIX-broker-restart-test-hardening.md`.

## Why this exists

Every long-running test package (`./tests/longrunning/...`) provisions
its broker / DynamoDB / SQS / Service Bus / RabbitMQ / Artemis fixture
through one of the `testutil/<svc>local` helpers. Those helpers shell
out to the host docker daemon via plain `exec.Command`:

```text
testutil/rabbitmqlocal/rabbitmqlocal.go:320: out, err := exec.Command("docker", "run", "-d", ...).CombinedOutput()
testutil/ddblocal/ddblocal.go:278:           cmd := exec.Command("docker", args...)
testutil/asblocal/asblocal.go (sql + emulator startup)
testutil/mqttlocal/instance.go
testutil/artemislocal/artemislocal.go
testutil/sqslocal/sqslocal.go
testutil/localstack/localstack.go
testutil/s3local/s3local.go
```

None of them use `exec.CommandContext`. If the docker daemon is
unhealthy (image pull stalls, daemon socket blocked, network
hiccup, low-disk hang), the helper blocks indefinitely. Because
the longrunning package timeout is `3h` (`make test-long-running`),
a single stuck `docker run` consumes the entire 3-hour budget and
prints no failure until the parent context cancels it — by which
point CI has already lost three hours of feedback and the rest of
the longrunning suite never executes.

The same pattern affects pre-flight `docker rm -f`, `docker
inspect`, and `docker logs` calls during teardown / readiness
probing.

## Decision

Wrap every docker invocation in `testutil/*local/*.go` with
`exec.CommandContext` and a bounded per-call timeout. Tunable per
helper, but the practical defaults are:

| Operation                     | Timeout |
|-------------------------------|---------|
| `docker run -d`               | 90 s    |
| `docker rm -f` / `docker stop`| 30 s    |
| `docker inspect` / `docker ps`| 10 s    |
| `docker logs`                 | 10 s    |
| `docker exec` (inside helper) | 30 s    |

90 s on `run -d` covers a cold image pull on a clean CI worker but
is well under the longrunning package timeout (3 h) and well under
any individual test timeout (240 s for UC42/UC43).

On timeout, return a wrapped error that clearly identifies the
docker subcommand, container/image name, and elapsed duration. The
calling test should `require.NoError` immediately so the failure
surfaces within minutes, not hours.

## Tasks

### 1. Add a small shared helper

Create `testutil/dockerexec/dockerexec.go`:

```go
// Package dockerexec wraps docker CLI invocations with a bounded
// per-call timeout so a stuck docker daemon cannot consume the
// entire test package timeout.
package dockerexec

import (
    "context"
    "fmt"
    "os/exec"
    "time"
)

// Run runs `docker args...` with the given timeout and returns
// combined stdout+stderr. The error wraps the subcommand and a
// "(timed out after X)" suffix when the deadline expires.
func Run(timeout time.Duration, args ...string) ([]byte, error) {
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()
    out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
    if ctx.Err() == context.DeadlineExceeded {
        return out, fmt.Errorf("docker %v: timed out after %s: %w", args, timeout, ctx.Err())
    }
    return out, err
}

// Default timeouts.
const (
    RunTimeout     = 90 * time.Second
    RemoveTimeout  = 30 * time.Second
    InspectTimeout = 10 * time.Second
    LogsTimeout    = 10 * time.Second
)
```

### 2. Migrate every `exec.Command("docker", ...)` call site

Inventory (regenerable with `rg "exec\.Command\(\"docker\""
testutil/`):

```
testutil/rabbitmqlocal/rabbitmqlocal.go:314,317,320,391,399,404,412,458
testutil/ddblocal/ddblocal.go:264,278,316,324,329,337,378
testutil/asblocal/asblocal.go:207,208,209,212,...
testutil/mqttlocal/instance.go:*
testutil/mqttlocal/helpers.go:*
testutil/mqttlocal/mqttlocal.go:*
testutil/artemislocal/artemislocal.go:*
testutil/sqslocal/sqslocal.go:*
testutil/localstack/localstack.go:*
testutil/s3local/s3local.go:*
```

For each call site:

```diff
- out, err := exec.Command("docker", "run", "-d", ...).CombinedOutput()
+ out, err := dockerexec.Run(dockerexec.RunTimeout, append([]string{"run", "-d"}, args...)...)
```

```diff
- _ = exec.Command("docker", "rm", "-f", name).Run()
+ _, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
```

```diff
- out, _ := exec.Command("docker", "logs", "--tail", "50", name).CombinedOutput()
+ out, _ := dockerexec.Run(dockerexec.LogsTimeout, "logs", "--tail", "50", name)
```

The teardown paths that swallow the error today (`_ = ...Run()`)
should still swallow the timeout — teardown best-effort is correct.
The provisioning paths that already check `err` will now also
report timeouts, which is the goal.

### 3. Cover the helper with a unit test

`testutil/dockerexec/dockerexec_test.go`:

1. `TestDockerexec_Run_Succeeds` — runs `docker version` (skip if
   docker is not installed via `t.Skip`).
2. `TestDockerexec_Run_TimesOut` — runs a deliberately slow command,
   e.g. `docker run --rm alpine sleep 30` with a 1 s timeout, and
   asserts the returned error wraps `context.DeadlineExceeded`.
   Mark as `//go:build longrunning` so it only runs when docker is
   already in scope.

### 4. Document

Add a `## Docker fixture timeouts` section to
`tests/longrunning/README.md` listing the per-call defaults so
future helper authors do not regress to plain `exec.Command`.

### 5. Verify

```bash
go test -race ./testutil/dockerexec/...
go test -race -timeout 1200s -v -tags=longrunning \
  -run 'TestGap_AMQP091_To_MQTT_CrossTransport|TestUC42_BrokerKillRestart_SharedOutbox|TestUC43_BrokerKillRestart_DirectHold' \
  ./tests/longrunning/...
```

Then `make test-long-running`. A simulated daemon stall (e.g.
`sudo systemctl stop docker` on a Linux CI) should now surface as
a clean timeout error in test output within ~90 s instead of a 3 h
hang.

## Acceptance

- Every `exec.Command("docker", ...)` in `testutil/*local/` has
  been migrated to `dockerexec.Run` (or
  `exec.CommandContext` directly with an explicit timeout).
- A grep `rg 'exec\.Command\("docker"' testutil/` returns no
  matches outside the new `dockerexec` package itself.
- Infra-startup failures abort the affected longrunning test in
  under 2 minutes.
- `make test` and `make test-long-running` are green.

## Non-goals

- Do not move docker provisioning out of test helpers into a
  separate orchestrator process. The current model
  (per-package fixture, owned by Go test) is fine.
- Do not introduce retry-on-timeout in `dockerexec.Run`. A timeout
  here means the host is unhealthy; failing fast is the desired
  signal.
- Do not bound non-docker exec calls (e.g. `make`, `mosquitto_sub`)
  in this pass. Scope it to docker.

## Related

- `testutil/rabbitmqlocal/rabbitmqlocal.go`,
  `testutil/ddblocal/ddblocal.go`,
  `testutil/asblocal/asblocal.go`,
  `testutil/mqttlocal/`,
  `testutil/artemislocal/artemislocal.go`,
  `testutil/sqslocal/sqslocal.go`,
  `testutil/localstack/localstack.go`,
  `testutil/s3local/s3local.go`.
- `Makefile` → `test-long-running` target (3 h package timeout).
- `tests/longrunning/README.md`.
