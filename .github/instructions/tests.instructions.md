---
applyTo: "**/*_test.go,tests/**,testutil/**"
---

# Tests

The contract is TESTS.md; its §9 checklist is the review list. `make test`
already audits new `time.Sleep` and `runtime.Gosched()` calls in tests, so those need no
comment. The rest below is not machine-checked.

- Each test is exactly one category — unit, integration or long-running — and
  sits where TESTS.md §1 puts it. Every file under `tests/longrunning/` carries
  `//go:build longrunning`. Integration tests are gated by `testing.Short()` and
  a `testutil/*local` Docker probe, never by a build tag.
- No `time.Now()` in a test; inject `domain/clock` and drive
  `clocktest.FakeClock`. Lint exempts `_test.go` from this, so review is the
  only check.
- Waits use `testutil/wait` (`Until`, `Poll`, `RequireReceive`, …) or
  `testutil/dockerexec`, never a hand-rolled poll loop.
- Every goroutine, file, container, env var and registry change is registered
  with `t.Cleanup` (or `defer`) when it is acquired. No `t.Parallel()` with a
  shared `*ports.Registry`, env vars or working directory.
- Fakes are hand-written in `fakes_test.go` and implement only the port they
  claim. gomock and mockery are not used.
- Errors are compared with `errors.Is` / `errors.As`; a `BridgeError` by `Code`
  and `Class`. Never by `err.Error()` text. Prefer asserting the whole value
  over a substring check — `"amqp.amqp091"` contains `"amqp091"`.
- Delivery tests assert `ports.OutboundMessage{Envelope, Address}` and never
  assume the address is in `Envelope.Subject`. Ingress tests check that
  `x-bridge.*` headers are stripped.
- A test names the behaviour it pins. A test named for a class ("every",
  "any", "never") must fail if that class were broken, including the near miss.
- No planning identifiers in test file names or in tokens the lint gate does
  not know (`T14`, a `S10` suffix, a status matrix of task rows). The gate
  reads test file contents but not file names (AGENTS.md).
- CDK tests never call `jsii.Close()`. It races `cmd.Wait()` and crashes the
  binary (TESTS.md §2.7). Do not suggest adding it back.
- Containers are named `gobridge-<package>-<uuid>` and started only through
  `testutil/*local` or `testutil/dockerexec`, never raw
  `exec.Command("docker", …)`. They are removed through `dockerexec.Remove`,
  never a `docker rm -f` of their own: without `-v`, removing a container
  leaves the anonymous volumes of its image's `VOLUME` paths behind.
- A package that uses a shared fixture (`mqttlocal.BrokerURL`,
  `ddblocal.Endpoint`, `flocilocal.Endpoint`, …) calls that helper's
  `Shutdown` in `TestMain` after `m.Run()` (TESTS.md §5.1). Nothing else
  removes a shared container, so a package without it leaves the container
  running after every run.
