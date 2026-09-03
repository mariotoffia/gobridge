# 0002 — Credential rotation: build-first, commit-after-success

Status: accepted
Date: 2026-07-03
Deciders: GoBridge core

## Context

A running adapter holds a live connection built from credentials. When those
credentials rotate — a refreshed SAS token, a new connection string — the
adapter must move to the new material without dropping messages or leaving the
connection in a half-updated state.

The naive path mutates the live config first, then rebuilds: set
`cfg.Connection = newCredentials`, then tear down and rebuild the connection
stack. If the rebuild fails (bad token, broker unreachable), the adapter is left
holding config that names credentials it never managed to connect with. The old,
working connection is gone and the recorded state lies about what is live.

## Decision

Build-first applies to swap-capable transports — those that hold a client handle
that can be rebuilt off to the side and swapped atomically (Azure Service Bus,
SQS). For those, never mutate live connection config before the replacement stack
has been built successfully: build first, commit the config only after the new
stack stands up, and fence the session-mode commit against a concurrent rebuild
with a generation counter.

Exclusive-connection transports that run a reconnect loop (paho MQTT, AMQP 1.0)
cannot build-first without opening a second concurrent connection, which their
single-session semantics forbid. They take the accepted alternative: commit the
new material, then force a reconnect and let the retry loop reconverge.

### Swap-capable client-handle transports

Reference implementation — Azure Service Bus rotation:

- `ApplyCredentials` (`adapters/azure/transport/servicebus/credentials_refresh.go`)
  takes the session-mode path that builds the new stack (`:224`) **before** it
  commits (`:233`). On a build failure, `cfg.Connection` is never touched and
  `rebuildPending` stays set, so the poll loop retries and the adapter
  self-heals rather than adopting broken config.

- The session-mode commit is generation-fenced: `commitRebuild(gen, ...)`
  installs the new stack and sets `r.cfg.Connection = conn` (`receiver.go`,
  `:225`) **only if** `r.rebuildGen == gen` (`:217`). `beginSessionRebuild` bumps
  `rebuildGen` (`receiver.go`), so a newer rotation that started while
  this one was building wins, and the stale build discards its result instead of
  overwriting fresher credentials.

- The non-session path also builds first, then swaps via an **unfenced**
  `commitStack` (`credentials_refresh.go`). No generation fence is needed
  there because no concurrent poll-loop rebuild exists on that path — the fence
  guards only the session-mode self-heal race.

SQS follows the same build-first shape without sessions: `ApplyCredentials`
rebuilds the SQS client with the new material and only then swaps it under the
init lock (`adapters/aws/transport/sqs/acl_credentials.go`). A rebuild
failure returns the error with the old client still live.

### Exclusive-connection reconnect-loop transports

paho MQTT and AMQP 1.0 hold one session-scoped connection and reconnect through
their own loop. They mutate live config under the session lock, then force a
reconnect:

- paho `Session.ApplyCredentials` swaps `liveCreds` / `opts` under `s.mu`, then
  calls `cm.Disconnect`; autopaho's loop reconnects with the new material
  (`adapters/mqtt/transport/paho/session_credentials.go`).
- amqp10 `Session.ApplyCredentials` swaps `liveCreds` / `opts`, then `conn.Close`
  triggers the monitor loop to redial
  (`adapters/amqp/transport/amqp10/session_credentials.go`).

Known failure mode: a bad rotation strands the session reconnect-looping on the
new (broken) material. There is no rollback — the previous credentials are
already overwritten, so the session stays down until an operator pushes a
corrected rotation.

## Consequences

- On a swap-capable transport, a failed rotation leaves the previous working
  connection and config intact; the adapter keeps running on the old credentials
  and retries the swap. Committed config always describes a stack that connected
  at least once.
- On a reconnect-loop transport, a failed rotation has no rollback: the session
  reconnect-loops on the new material until corrected. This is the accepted cost
  of single-connection exclusivity.
- Concurrent session-mode rotations on Service Bus are safe: the generation fence
  discards a slow build whose result is already stale, so credentials never move
  backward.
- Each adapter carries its own ordering. A swap-capable adapter that mutates
  config before building reintroduces the half-updated failure mode; adapter
  reviews must check for it.

## Rejected alternatives

- **Mutate config, then rebuild, on swap-capable transports.** Simpler to write,
  but a rebuild failure strands the adapter on unreachable credentials with the
  working connection already discarded. Rejected for the transports that can
  build off to the side — it fails unsafe where a safe path exists.
- **Force build-first onto reconnect-loop transports.** Would require a second
  concurrent connection during rotation, which single-session exclusivity
  forbids. The commit-then-reconnect alternative is the structural fit; its lack
  of rollback is documented rather than engineered away.
- **Global rotation lock across adapters.** Serializes every credential refresh
  through one mutex. Unnecessary: the per-adapter generation fence already makes
  concurrent rotations of the same adapter safe, and rotations of different
  adapters are independent.
