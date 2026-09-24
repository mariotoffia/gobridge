---
applyTo: "runtime/**,bridge/**,adapters/native/cluster/**,adapters/aws/cluster/**,deployment/aws/lib/bootstrap/**"
---

# Runtime, composition root and clustering

Sources: ADR-0001, ADR-0004, ADR-0009, ADR-0012 to ADR-0015, ADR-0017 to
ADR-0019, `docs/internals/architecture-message-flow.md`,
`docs/internals/architecture-contracts-and-clustering.md` and
`docs/cluster/spec/cluster-config-rollout-protocol.md`.

## Ingress and dispatch

- Reserved headers are stripped in one place, `doHandleDelivery`
  (`StripReservedHeaders`), before anything reads headers. A new consumption
  path runs downstream of it or strips for itself. A trusted signal rides a
  typed field, never a new `x-bridge.*` header (ADR-0001).
- `DispatchPlan.Address` goes on `OutboundMessage.Address`; it is never written
  into `Envelope.Subject`.
- `shared_outbox` acks the source only after `Persist`; the drainer calls
  `Complete` only after the send returns (ADR-0009).
- `direct_hold` in-process retry (ADR-0017): each send has its own
  `send_timeout`; waits come from the backoff ladder with a 100 ms floor; a
  `RetryAfter` hint is not jittered. It stops on success, a non-recoverable
  error, context end (leave unsettled, spend no replay budget), wedge, or budget
  exhaustion — and re-checks every stop condition when a wait ends.
- Completing work after a successful send (`OutboxStore.Complete`, a DLQ
  write, a settlement) runs on `context.WithoutCancel` plus a bounded timeout.
  Inheriting the batch or request context lets it expire after the send and
  turns a delivered message into a duplicate.
- Any retry loop has a floor on its wait. A zero `RetryAfter` or a 1 ms backoff
  otherwise becomes a tight loop of thousands of sends.
- `InjectRedrive` / `InjectToBinding` mint a fresh ID with a causation link,
  strip `x-bridge.dedup-id`, the generated-id marker and the receive counters,
  and refuse a missing binding with `ErrNotFound` before the pipeline runs —
  never a fallback to fan-out. A terminal non-delivery returns
  `ports.ErrInjectNotDelivered` (ADR-0015).

## Automatic DLQ redrive (ADR-0019)

- The subscription-added hook never takes `rt.mu`: the session calls it from
  inside a reconcile. It only starts the pass through `startBackground`, and
  the pass returns nil, so a failed redrive never makes the runtime terminal.
- The hook is installed only with a DLQ store, a window above zero, and a
  session that reports a non-empty managed subscription identity.
- A pass redrives inject-then-delete through `injectRedrive` with hold set, so
  a failure the route would hand back to its source is returned instead of
  dead-lettered as a second record. The delete after a confirmed inject runs
  on `context.WithoutCancel` plus a bound.
- One pass at a time: `autoRedrive.mu` is held for the pass, not for the
  readiness wait.
- A pass lists with `Before` set to its start plus one millisecond (an
  exclusive bound over millisecond-precision stores), so records written later
  in the pass wait for the next event and one written in its own millisecond
  is still included.
- The automatic inject recovers a panic into a failure (counted, audited,
  record kept, pass stopped); a synchronous inject has no per-delivery
  recover of its own.
- An error wrapping `ports.ErrInjectNotDelivered` moves the pass to the next
  record; any other error stops it. A failed record keeps its `RedriveMode`
  and `ExtraInfo`.

## Lifecycle

- A runtime starts once, stops once and is never restarted: `Start` on a
  stopped runtime errors. A change confined to reload units retires and grafts
  units inside the running runtime (ADR-0018); any other change builds a new
  instance. A reload wedges the process (`Terminal()`) only when a swap and its
  recovery both fail, when a runtime that must stop before its replacement
  starts does not stop cleanly, or when a unit an in-place reload retires does
  not stop cleanly (ADR-0004).
- Lease lifecycle: acquire, renew, step down after `MaxRenewFails` or
  `STALE_FENCING_TOKEN`, wait `StepDownGrace`, release. All of it is derived
  from `LeaseTTL` and driven by the injected `Clock`. A lease-owning session
  that cannot renew escalates to `ErrSessionUnrecoverable`.
- `bridge.Builder.Plan` rejects a second receiver on a
  `CapDedicatedIngressSession` session, or a second route on its receiver,
  before opening any resource. The check reads the capability, never the
  transport name.
- Every instance ID must be unique. The runtime does not validate this, and a
  duplicate breaks fencing.
- A predicate over `cfg.Sessions` filters by `referencedSessionIDs` exactly as
  the builder does; an unreferenced session is never built. "Which sessions are
  exclusive" has one answer, `exclusiveSessionIDs`, which includes sessions
  named by a route binding.
- A reload that changes a session between exclusive and non-exclusive stops
  the old consumer before starting the new one; overlap runs both on one
  identity.

## In-place reload (ADR-0018)

- `InPlaceReload.Apply` preflights the whole next document and plans every
  added unit's part before it retires anything. Each root first runs every
  check its full swap runs — the Supervisor's no-op detection, cluster guard,
  store-identity, lease `session_id` and durable-backlog preflights; the AWS
  runtime's fingerprint, deployment-profile admission and cluster seam — so an
  in-place reload never admits what a full swap refuses.
- When `RequiresSerializedSwap(retired, added)` holds, every retired unit stops
  before any part is built; otherwise parts are built first and a failed build
  changes nothing. A graft always follows the retires, so one runtime never
  runs two route runners for a route id or two managers for a session id, and
  an exclusive identity never has two holders.
- `PlanInPlaceReload` refuses when a bridge-wide section (`bridge`, `stores`,
  `config_watch`, `http`) differs, or a retired or added unit attaches to a
  `CapHTTPEndpoint` transport or one with no registered factory. A unit key
  covers only the unit's members plus those sections, so a value a build
  derives across units must also make it refuse when it differs, as the outbox
  stale-claim duration does; otherwise the runtime keeps the value it was built
  with.
- A part is built over the host's `Stores()` with `WithSharedStores` (`Graft`
  refuses any other), and neither a grafted nor a discarded part closes them.
- `Graft` refuses a part that reuses a route or session id, the runtime's or
  one of a unit still being retired (a straggler may still run under it), is
  not closed over its sessions (a route on either side rides on or binds to a
  session of the other), or fails the route checks `Start` runs over the union.
- `Retire` settles in-flight deliveries within the stop drain budget before it
  cancels, keeps the unit reachable by `Fence` and by DLQ fencing on its
  sessions' leases until its runs finish, and closes its managers only after
  its runs finish or the store-close grace ends. `Apply` bounds each retire by
  the drain timeout, detached from the caller's cancel.
- A credential refresher watches a receiver or sender only when the runtime
  holds it (`CredentialTargets`). `Retire` hands the unit's targets to every
  forget and closes a refresher left watching nothing.
- A build or graft that fails after the retires restores the retired units
  from the running configuration (`unchanged`), and reports `torn` only when
  that restore fails. Both roots act on the outcome alike: `unchanged` keeps the runtime;
  `torn` stops it and rebuilds the running configuration, wedging if the stop
  or the rebuild fails; `wedged` stops it and wedges without building anything.
- A serialized reload wedges, rather than restore, when a part built for it
  does not stop: that part may still hold the exclusive identity a restored
  unit would claim. It wedges, rather than tear, when a restored part whose
  graft is refused does not stop, as the torn rebuild would claim its identity.
  A build-first part claims none, so its stop failure leaves the reload
  `unchanged`.

## Timers, waits and locks

- `time.Duration` arithmetic can overflow. Compare by subtraction
  (`budget - elapsed < timeout`), not by adding two large durations, and
  saturate instead of wrapping negative.
- State is re-checked after every wait ends. A timer only guarantees a minimum
  delay, and a wedge, a stop or a cancel can latch while it runs.
- A metric or health value is read under the same lock as the state it
  reports. A snapshot taken before the unlock reports a stale value.

## Cluster config changes

- A live reload into or out of a clustered deployment that is not a no-op is
  rejected in both `Supervisor.apply` and `App.applyLogicalConfig`, after no-op
  detection and before build. `WithAllowDestructiveReload` does not bypass it
  (ADR-0012).
- The guard lifts only for `bridge.cluster.rollout: coordinated` with a
  live-safe delta. Members swap only after observing `Committed`, every decision
  re-reads with `ConsistentRead`, a node rejects an `(epoch, generation)` lower
  than one it applied, and a new coordinator waits one lease duration before its
  first side effect (ADR-0013).
- A coordinated root wires `config.Validate` and re-syncs the config manager
  after a barrier swap (`Manager.AdoptRunning` / `NotifyApplyResult`).
- Confirm window (ADR-0014): `Confirmed` is written only inside the window,
  with the deadline checked before the barrier. The last-committed artifact
  advances only on `Confirmed`. Revert is whole-cohort.
- Post-commit apply failures retry with capped backoff and go terminal past the
  attempt bound (ADR-0013).
