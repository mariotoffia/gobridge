# 0004 — Single-use runtime lifecycle and terminal wedge

Status: accepted
Date: 2026-07-03
Deciders: GoBridge core

## Context

A runtime instance owns transports, stores, and route runners. Restarting a
stopped instance in place would have to reset every one of those, and any missed
reset leaks a goroutine, a connection, or a stale lease. The file-based
deployment also swaps configuration by replacing the runtime instance, which
raises a harder question: what happens when the swap fails and recovery to the
previous runtime also fails? The process is then holding no working runtime and
cannot build one.

Health checks make this concrete. If `/live` returns 200 while the process holds
no functioning runtime, the orchestrator keeps routing traffic to a task that
can never serve it. The failure must be visible to the orchestrator so it
restarts the task.

## Decision

A runtime is single-use, and a wedged bootstrap is terminal — the process exits
and `/live` fails closed.

- **Start-once, stop-once.** `Start` on a stopped runtime returns an error
  rather than resetting state: `"runtime: cannot start a stopped runtime
  (single-use lifecycle); build a new runtime"`
  (`runtime/bridge_start.go:44-46`). Configuration changes replace the instance
  (swap mode); they never restart one.

- **Terminal state on the port.** `ports.Runtime` exposes `Terminal() bool`
  (`ports/runtime.go:158`). A runtime that can never serve again reports
  `Terminal() == true`.

- **Wedge = swap failed AND recovery failed.** In the file-based bootstrap, the
  process is WEDGED only when a prepare/commit swap failed **and** the recovery
  back to the previous runtime also failed (`wedged atomic.Bool`,
  `deployment/aws-filebased-config/lib/bootstrap/app.go:145-154`). `Run` exits
  non-zero once terminal (`ErrRuntimeTerminal`, `app.go:34`), driven by a
  terminal backstop poll (`defaultTerminalPollInterval = 5s`, `app.go:39`).

- **Fail closed via a sentinel.** A `terminalRuntime` stands in for the wedged
  instance (`bootstrap/terminal_runtime.go`): `Terminal() == true`,
  `Healthy() == false`, `ReadinessLevel == LevelDown`, and `ComponentErrors`
  carrying `{"bootstrap": errRuntimeWedged}`. `/live` returns 503 when
  `rt != nil && rt.Terminal()`, so a wedged process fails its liveness probe
  with no change to the httpapi layer.

## Consequences

- No in-place restart path exists, so no half-reset runtime can leak resources.
  A config change builds a fresh instance or it fails.
- A wedged process exits non-zero and fails `/live`. The orchestrator restarts
  the task from a clean slate instead of holding a dead one behind a green
  health check.
- The terminal sentinel keeps httpapi ignorant of bootstrap internals — the
  health handler reads `Terminal()` and `Healthy()`, nothing more.
- Recovery still runs first. Terminal is the state after both the swap and its
  recovery have failed, not the state after a single failed swap. A recoverable
  swap failure rolls back to the previous runtime and stays live.

## Rejected alternatives

- **Restartable runtime.** Requires resetting every owned resource on restart;
  one missed reset is a leak or a stale lease. Rebuilding a fresh instance is
  the safer default.
- **Keep serving on a wedged runtime with a degraded flag.** Leaves the
  orchestrator routing traffic to a task that cannot serve it. Failing `/live`
  closed and exiting hands the problem to the orchestrator, which is built to
  replace dead tasks.
- **Wire `EnsureAlarms`/self-restart inside the runtime.** Deliberately not
  done — process exit plus orchestrator restart is the supervision boundary. The
  runtime signals; the orchestrator acts.
