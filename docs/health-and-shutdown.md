# Health Checks and Graceful Shutdown

Health, liveness and readiness probes on the monitor server, and the shutdown
sequence and budgets a clean container lifecycle depends on. Split out of
[Deployment Guide](deployment-guide.md), which covers how the process is
delivered and configured.

GoBridge exposes health, liveness, and readiness probes on the monitor server,
plus configurable shutdown behavior for clean container lifecycle management.

### Health Endpoints

All health endpoints live on the monitor server (default `:8081`) and are
**unauthenticated** so orchestrators can probe them without credentials.

| Endpoint | Purpose | Healthy | Unhealthy |
|----------|---------|---------|-----------|
| `GET /api/v1/monitor/health` | Coarse health | 200 `{"status":"ok"}` | 503 `{"status":"unhealthy"}` |
| `GET /api/v1/monitor/live` | Liveness probe | 200 `{"status":"alive"}` | 503 `{"status":"terminal"}` once terminal |
| `GET /api/v1/monitor/ready` | Readiness probe | 200 `{"status":"ready"}` | 503 `{"error":"not ready"}` |

The `health` endpoint is coarse: HTTP 200 `{"status":"ok"}` when the runtime is
running and no critical background component has failed, and HTTP 503 otherwise
with `status` one of `unhealthy` (a background component failed), `not_running`
(paused or not yet started), or `unavailable` (runtime not wired). It does **not**
reflect broker/session connectivity or subscription state -- a broker outage
keeps `/health` green so a transient reconnect does not restart the pod. Gate on
connectivity with `/deephealth` or `/ready?level=connected` (see below). The
`live` endpoint returns 200 while the process is running and the runtime is still
recoverable (including before the runtime is wired **and after a deliberate admin
stop**), and 503 `{"status":"terminal"}` only once the runtime has entered a
terminal, unrecoverable state — so an orchestrator restarts the task instead of
leaving it wedged. The bare `ready` endpoint returns 200 once the runtime is
started and healthy **and carrying at least one route or session**; it does
**not** guarantee transport sessions are connected or subscriptions
acknowledged. Gate production traffic with the `?level=` parameter described
under "Readiness levels" below.

Two states deserve calling out because they look healthy from the outside:

- **Empty.** A bridge that carries no routes and no sessions bridges nothing.
  It happens when the configured file is missing and the process fell back to
  starting empty, or when a config defines no routes. `/live` stays 200 (the
  process is fine and must not be restarted), but `/ready` answers 503 and
  `/deephealth` reports `"empty": true` with the readiness level pinned at
  `running`. Nothing steers traffic at it and no rollout gate treats it as a
  converged member. If starting empty is never acceptable for a deployment,
  the reference binary accepts `-start-empty=false`, which turns a missing
  config file back into a fatal startup error.
- **Wedged.** A reconfiguration swap and its recovery both failed, so the
  process holds no active runtime and routes nothing. This is reported through
  the supervisor's own terminal state, so `/live` answers 503 immediately
  rather than waiting for a coarse background backstop, and the orchestrator
  restarts the task.

### Shutdown Timeouts

```yaml
bridge:
  shutdown_timeout: 30s
  # Scaled drain formula (preferred in production). The per-batch
  # ceiling is min(batchCount * per_record_drain_timeout,
  # max_drain_timeout). It only ever RAISES a batch budget that is
  # already floored at one full send.
  per_record_drain_timeout: 3s
  max_drain_timeout: 30s
```

| Field | Default | Description |
|-------|---------|-------------|
| `shutdown_timeout` | `30s` | Total grace period for clean shutdown. In the file-based deployment it IS the process budget: the config watcher join, the rollout-drive stop, the HTTP shutdown, the runtime drain, store close and telemetry flush all run inside it. Read from the **running** configuration when shutdown starts, so a value raised through a reload takes effect without a restart. |
| `drain_timeout` | `30s` | Ceiling on `Runtime.Stop` when the supervisor stops a runtime (shutdown or reconfiguration swap). Not a per-batch outbox budget. |
| `per_record_drain_timeout` | `3s` | Per-record budget in the scaled formula. |
| `max_drain_timeout` | `10s` | Absolute ceiling for the scaled formula. |

The shutdown sequence proceeds as follows:

1. **Signal received** -- `cmd/gobridge` catches `SIGINT` or `SIGTERM` and
   cancels the root context. A second signal forces an immediate exit with
   code 2. Cancelling that context does **not** reach in-flight deliveries: the
   runtime runs its routes, receivers and senders on a context detached from the
   caller's, so the only thing that cancels work is `Runtime.Stop` -- the
   settle-then-cancel sequence in steps 3--4. If nobody calls `Stop`, cancelling
   the start context drives one itself, under the same `drain_timeout` ceiling.
2. **Readiness goes false** -- The runtime marks itself not-ready, so `/ready`
   fails and a load balancer stops routing new **push** traffic to the instance.
   **Pull** receivers such as the SQS poller are not gated by readiness and keep
   pulling from the broker until the context is cancelled in step 4.
3. **Settle in-flight** -- Before cancelling anything, the runtime settles
   already-accepted in-flight deliveries (send + ack) up to a bounded budget:
   `WithStopQuiesce` if set, otherwise a 25s ceiling, and never longer than the
   `drain_timeout` the supervisor allots. If the budget expires, any unsettled
   source falls to broker redelivery (at-least-once) -- it is never silently
   acked. This settle phase runs by default, not only under `WithStopQuiesce`.
4. **Cancel and drain** -- The runtime context is cancelled, tearing down
   receiver loops; the credential refresher closes, then the outbox drainers get
   a bounded grace to finish so a final drain's `Complete` runs against a live
   lease.
5. **Close transports and stores** -- Session managers close (releasing leases),
   then unmanaged sessions, then durable stores, then telemetry -- each under a
   bounded close timeout detached from the caller context.
6. **Shutdown HTTP** -- After the supervisor finishes, `cmd/gobridge` stops the
   admin, monitor, and transport HTTP servers, bounded by `shutdown_timeout`.
7. **Exit** -- The process exits with code 0 on a clean shutdown.

In the file-based deployment the same budget also covers the stages *before* the
runtime drain: the config-watcher join and the coordinated-rollout drive stop are
waited on **selectably** against it. A reload stuck in its own teardown, or a
barrier lease store that will not release, is abandoned when the budget runs out
(logged) rather than holding `SIGTERM` ahead of the drain, the HTTP shutdown and
the metrics flush until the platform's SIGKILL.

`Runtime.Stop` is idempotent and single-teardown: exactly one caller performs
the teardown and every other caller blocks on it and then receives **that
teardown's result**. Two callers race on every signal (the start-context watcher
and the composition root's own stop), so this is what keeps a "bridge stopped"
log line from covering a drain that actually failed.

The in-flight settle (step 3) is bounded by `drain_timeout`; the subsequent
cancel and close phases (steps 4--5) run detached from the caller context under
their own bounded close timeouts. The process then has `shutdown_timeout` to
finish HTTP cleanup. Set `drain_timeout` shorter than `shutdown_timeout` to
leave headroom. When the drain budget expires before
in-flight work settles, durable outbox records stay persisted and at-least-once
sources are redelivered by the broker on restart -- remaining work is not silently
dropped.

A `Stop` error names the phase that failed -- background components still
running at the deadline, a session manager, a named unmanaged session, a
role-tagged store handle, or the metrics flush -- because the remedies differ:
raise `drain_timeout` or shed ingress earlier for the first, chase a plugin for
the rest.

**A failed stop during a reconfiguration swap is terminal.** `Runtime.Stop` has
no early error return, so by the time it reports a failure it has already
cancelled its work context and closed its managers, sessions and stores, and a
stopped runtime is single-use. The supervisor (and the file-based bootstrap app)
therefore **wedges** rather than keeping the torn-down runtime installed:
`/live` fails closed and the orchestrator restarts the task with freshly-built
transports, which is also the only thing that clears hung plugin residue. See
[ADR-0004](adr/0004-single-use-runtime-lifecycle.md).

### Exit Codes

An orchestrator reads the process exit code to decide whether to restart. The
two binaries use these codes.

**`cmd/gobridge`** (`cmd/gobridge/main.go`):

| Code | Meaning |
|------|---------|
| `0` | Clean shutdown — `SIGINT`/`SIGTERM` handled, or the supervisor self-exited without error (`main.go:315,348`). |
| `1` | Startup failure (plugin registration, config load, watcher start, HTTP server start, or the supervisor produced no runtime), or the runtime entered a terminal, unrecoverable state (`main.go:70,77,106,202,223,278,331`). |
| `2` | Flag/usage error (Go `flag` package default `ExitOnError`), or a second `SIGINT`/`SIGTERM` forcing an immediate exit before drain completes (`main.go:339`). |

**`gobridge-filebased`** (shipped image entrypoint, `deployment/aws-filebased-config/lib/cmd/gobridge-filebased/main.go`):

| Code | Meaning |
|------|---------|
| `0` | Clean shutdown, or the `-healthcheck` probe found the monitor liveness endpoint returning 200 (`main.go:40,66`). |
| `1` | Bootstrap config load failure, `app.Run` returned an error, or the `-healthcheck` probe failed — endpoint not 200 or unreachable (`main.go:40,46,65`). |

`app.Run` reports the runtime drain's real result, so a shutdown whose drain
overran its budget or whose transport refused to close exits `1`, not `0`. That
is deliberate: the alternative was a "bridge stopped" line over a teardown that
had in fact failed. Size `drain_timeout` for the busiest expected in-flight set
if a rolling restart must exit `0`.

A terminal runtime exits non-zero (`cmd/gobridge` → `1`) precisely so a Kubernetes
`livenessProbe` or ECS health check restarts the task rather than leaving it
wedged.

**A restart policy is required — the process is designed to exit and be
restarted.** GoBridge follows a let-it-exit recovery model: several paths end by
*exiting non-zero on purpose* rather than wedging in place. The clearest is a
single-use exclusive session that steps down from its lease and cannot reacquire
it — it reaches a terminal state and the process exits (recovery leg 5; see
[ADR 0004](adr/0004-single-use-runtime-lifecycle.md) and the Scenario 8 backstop
note). This is safe **only** when something restarts the process so it can
re-elect or reconnect. Kubernetes Pods (`restartPolicy` defaults to `Always`)
and ECS services restart automatically, but a **bare `docker run` without
`--restart` stays down** after such an exit. For any long-lived container
deployment, set a restart policy explicitly:

```bash
docker run --restart unless-stopped ... your-gobridge-image
```

For `docker compose`, set `restart: unless-stopped` (or `always`) on the
service; for systemd units, `Restart=always`. Only a plain `docker run` /
`docker compose` without an explicit restart policy needs this called out —
orchestrators already restart by default.
