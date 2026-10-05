# 0020 — Contain an unrecoverable session by rebuilding its reload unit

Status: accepted
Date: 2026-10-05
Deciders: GoBridge core
Relates to: [0004](0004-single-use-runtime-lifecycle.md) (the process restart
stays the backstop for every failure a rebuild must not answer),
[0018](0018-reload-in-place-by-unit.md) (a rebuild is a serialized in-place
reload of one reload unit)

## Context

A session manager that ended with `session.ErrSessionUnrecoverable` made the
runtime terminal: the process exited and the orchestrator started a new one.
That is the right answer when work of the old session may still run. Most of
the time, though, only one session had failed, and every other session and
route in the process stopped with it.

These MQTT faults end that way. Each one latches the paho session closed, and
each one reaches the supervisor as `ErrSessionUnrecoverable` wrapping
`shared.ErrTransportClosedPermanently`:

1. **Ingress does not quiesce before a recycle.** When the managed-subscription
   cleanup of a persistent or exclusive session removes a stale filter, the
   session stops ingress and waits for route work to settle the deliveries it
   holds before it disconnects. The wait is bounded by
   `sessions[].options.session.reconcile_timeout`. When route work still holds
   deliveries at that bound, the session fails closed.
2. **Settlement recovery fails.** A recovery recycle that cannot complete fails
   the session closed: when its drain of in-flight work fails, and also when
   the drain succeeds and a later step — the reconnect, the Session Present
   check, the reconcile — fails
   ([MQTT settlement recovery](../transports/mqtt-settlement-recovery.md)).
   [ADR 0021](0021-contain-mqtt-recovery-and-ingress-reject-in-session.md) now
   decides the transport side: a failure after a successful drain no longer
   latches the session closed, unless the failing step fails closed the way it
   does outside a recovery (managed-subscription cleanup). A failed drain, or a
   failure before the drain, still ends this way.
3. **Ingress poison.** A packet the pre-decode guard rejects — malformed MQTT
   structure, or larger than the advertised Maximum Packet Size — latches the
   session closed ([MQTT ingress poison](../runbooks/mqtt-ingress-poison.md)).
   [ADR 0021](0021-contain-mqtt-recovery-and-ingress-reject-in-session.md) now
   decides the transport side: the reject drops only the connection, and the
   session reconnects without going terminal.

Any other path that latches the marker ends the same way, for example the
pinned replay of a removed filter on a runtime with no dead-letter store.

A single-use exclusive session that wins its lease back after an ordinary
step-down ends the same way: `Start` after `Close` returns the permanent marker
([Scenario 8](../scenarios/08-clustered-exclusive-sessions.md#connect_after_lease-true)).

The paho session is single-use, so the failed session can never start again.
A fresh session can: the builder makes one from the running configuration, and
ADR 0018 already replaces a reload unit inside a running runtime.

## Decision

**A session that fails in a way a fresh session clears is rebuilt in place.
The composition root rebuilds the one reload unit that holds the session; the
rest of the runtime keeps running.**

### The runtime reports the session

- `runtime.WithSessionUnrecoverableHandler(h)` installs a handler
  `func(sessionID string, cause error) bool`. `bridge.WithSessionUnrecoverableHandler`
  forwards it to the runtime a full build makes. A part built for an in-place
  reload needs none: its sessions run under the host runtime's supervisor.
- A failure is **rebuildable** when it carries
  `shared.ErrTransportClosedPermanently` and does not carry
  `session.ErrProcessRestartRequired`.
- With a handler installed and a rebuildable failure, the session supervisor
  records the fault, waits a backoff, then calls the handler:
  - the backoff is per session: 1 s, doubling to 30 s, with equal jitter (each
    wait lies between half and all of its step). It starts again at 1 s after
    any run of the session that stayed up for 30 s, however that run ended.
    The runtime keeps it across rebuilds, so a session that fails again after
    a rebuild waits longer;
  - `true` means the root took the rebuild. The supervisor counts
    `SessionRebuilds` (tagged `session_id`) and ends without making the runtime
    terminal;
  - `false` makes the runtime terminal, as before;
  - a shutdown during the backoff ends the supervisor without calling the
    handler and without making the runtime terminal.
- The runtime marks the report pending before it calls the handler, and clears
  the mark when the handler refuses or a retire of the session clears its
  fault; `(*runtime.Runtime).SessionRebuildPending` reads it. A fault recorded
  during the backoff, and one that needs a process restart, is never pending.
- Until a rebuild or retire clears it, the recorded fault (`session:<id>` in
  `ComponentErrors`) keeps the session not ready. Deep health reports it
  `ready: false` with `service_level: none`, and readiness does not excuse it
  as a deferred-connect standby, even when the session waits for its lease
  before it connects. Once a rebuild retires the unit, deep health lists
  neither the session nor the unit's routes until the fresh copy is added, as
  for any unit an in-place reload replaces; the unit attaches no HTTP
  endpoint, so no request the instance takes reaches it. The fresh session
  reads not ready until it connects.
- The handler runs on the session's supervisor goroutine. It must not block,
  and it must not retire the unit or reload synchronously, because a retire
  waits for that goroutine. It hands the rebuild to a goroutine of its own.

### The composition root rebuilds the unit

`bridge.Supervisor` (`cmd/gobridge`) and the AWS runtime's `bootstrap.App`
install a handler on every runtime they build. The handler:

1. plans the rebuild with `bridge.PlanSessionRebuild`: an in-place reload of the
   running configuration onto itself that retires the reload unit holding the
   session and adds a freshly built copy. It returns `false` when no unit holds
   the session or the unit may attach an HTTP endpoint (see ADR 0018). While a
   reload holds the root's apply lock, the handler takes the report without
   planning and step 3 decides: that reload may run a session the configuration
   it publishes last does not hold yet;
2. runs the rebuild on its own goroutine under the root's apply lock — the
   Supervisor's lifecycle lock, the App's apply lock — so a rebuild and a
   configuration reload never run at the same time;
3. re-checks under the lock that the runtime still runs and that the session's
   report is still pending (`SessionRebuildPending`), and plans the rebuild
   again against the configuration the lock now guards. When the runtime has
   changed or the report is no longer pending, a reload or a stop got there
   first, and it does nothing. So a report that waited for the lock rebuilds
   neither a session a reload put in its place, before that session's own
   backoff ends, nor a session whose fault needs a process restart. The
   Supervisor also does nothing once it shuts down; the App does nothing once
   its configuration is withdrawn, it is wedged, or it shuts down;
4. applies the plan with `(*bridge.InPlaceReload).Apply`. The plan is always
   serialized: the old unit is fully retired — its route work stopped, its
   sessions closed — before the copy is built and connects, so the old and the
   new session never hold the broker identity at the same time.

Every other unit keeps running untouched. A rebuild is not a reload: the applied
configuration, the registry and the API keys stay as they are, and it records
no reload metrics, emits no `SwapEvent` and starts no convergence watch. The
root logs its outcome with the retired and added routes and sessions.

### Restart-required failures carry a negative marker

`session.ErrProcessRestartRequired` marks an `ErrSessionUnrecoverable` that a
rebuild inside the process must not answer, because work of the old session may
still be parked or running, or because the process must not compete for the
lease again. The sites that wrap it are:

- a reconnect `Reconcile` that ignored its context past its ceiling;
- a lease lost during post-acquire activation before source work quiesced;
- a post-acquire activation failure when the activation or the source close did
  not complete;
- a step-down whose source close did not complete;
- session-failure recovery whose source close ignored its context;
- a step-down because this owner's broker path stayed non-converged
  (`broker_health_step_down`): the process must not compete for the lease
  again;
- a hand-off after a permanent close whose source close did not complete.

The marker says "restart", not "rebuildable", on purpose. `errors.Is` finds a
marker anywhere in the chain, so a positive "rebuildable" marker would survive
a restart-required site that wraps an already-marked error, and that error
would still read as rebuildable. With the negative marker, a wrap by a
restart-required site always reads as restart-required. The cost is the other
direction: a new site that forgets the marker reads as rebuildable. Review
enforces it: every site that returns `ErrSessionUnrecoverable` while old work
may be parked or running, while a close is incomplete, or when the process must
not compete for the lease again MUST wrap `ErrProcessRestartRequired`.

### Leases

The rebuild changes no lease rule. A session manager whose reconcile returns
the permanent marker, as when ingress did not quiesce (fault 1), keeps its
lease, since route work may still hold deliveries, and the retire leaves it
held. Any other failure releases the lease once the source closed. A failure of
the running session first waits the settlement grace a step-down waits
(`StepDownGrace`), and a send that completes after it is the duplicate a
step-down accepts. The rebuilt session competes for the lease only after the
old unit is retired, so its route work has stopped. A failure whose source
close did not complete keeps the lease and carries `ErrProcessRestartRequired`.

### When the process still stops

The runtime still goes terminal, and the process restart (ADR 0004) stays the
backstop, when:

- no handler is installed: a `runtime.Runtime` an embedder builds without
  `WithSessionUnrecoverableHandler`;
- the failure carries `ErrProcessRestartRequired`;
- the failure does not carry `shared.ErrTransportClosedPermanently`, for
  example an exclusive activation that overran its deadline after its source
  closed;
- the root has no unit to rebuild: the unit may attach an HTTP endpoint, or the
  running configuration changed between the report and the rebuild;
- the rebuild leaves the fault in place;
- a retired unit does not stop cleanly (`wedged`). The retire lets the unit's
  in-flight deliveries settle for up to 25 s before it cancels them, all inside
  the drain timeout. A delivery still held at that point wedges the rebuild when
  its sender ignores the cancel, or when the drain timeout leaves the route no
  time to stop after the cancel: a `drain_timeout` of 25 s or less.

A rebuild that tears (`torn`) is handled like a torn reload: the root stops the
runtime and builds the running configuration afresh, and wedges when that fails.

## Consequences

- One failed session no longer stops the process. Its reload unit — the session
  and every receiver, sender, binding and route joined to it — restarts after
  the backoff; every other unit keeps running.
- Under the Supervisor or the AWS runtime, a single-use exclusive session that
  wins its lease back after an ordinary step-down gets a fresh session in the
  process instead of a process restart. A broker-path step-down still restarts
  the process.
- `SessionRebuilds` counts the rebuilds a handler took. The failed session reads
  not ready until a rebuild or retire clears its fault.
- Known risk: a session that keeps failing is rebuilt again and again, at most
  30 s apart, and can keep its lease through it. A broker that keeps sending a
  malformed packet, or a pinned replay for a removed filter on a runtime with no
  dead-letter store, fails every fresh session the same way. Alert on the
  `SessionRebuilds` rate and on the session's readiness. (Since
  [ADR 0021](0021-contain-mqtt-recovery-and-ingress-reject-in-session.md) a
  malformed packet no longer fails the session; the guard drops only the
  connection.)
- Every failure a rebuild must not answer still ends in a process restart, so a
  restart policy is still required
  ([exit codes](../health-and-shutdown.md#exit-codes)).
- An embedder that builds `runtime.Runtime` directly keeps the old behaviour
  unless it installs a handler.

## Out of scope

The transport side of faults 2 and 3 is unchanged. A settlement recovery that
fails after a successful drain, and an ingress-poison rejection, still latch the
session closed, and only the rebuild above brings the session back. Letting the
MQTT transport recover from those without closing the session is a separate
decision, recorded in its own ADR when it lands.

That decision is now
[ADR 0021](0021-contain-mqtt-recovery-and-ingress-reject-in-session.md). A
settlement recovery that fails after a successful drain is abandoned, and a
pre-decode reject drops only the connection. Neither latches the session
closed any more; a failed drain, a recovery that fails before its drain, and a
step after the drain that fails closed the way it does outside a recovery
(managed-subscription cleanup) still do. A lease-managed session that an
abandoned recovery leaves with no connection is closed by its session manager
and still gets the rebuild above.

## Rejected alternatives

- **Restart the failed session in place.** The paho session is single-use, and
  resetting a closed session is the leak ADR 0004 guards against.
- **Replace only the session.** The unit's receivers and senders hold the dead
  session instance. The reload unit is the smallest thing ADR 0018 replaces.
- **Rebuild on the supervisor goroutine.** A retire waits for that goroutine, so
  the rebuild would wait for itself.
- **Rebuild without a backoff.** A session that fails as soon as it connects
  would be rebuilt in a tight loop.
- **A positive "rebuildable" marker.** A restart-required site that wraps an
  already-marked error would leave it rebuildable; see above.
