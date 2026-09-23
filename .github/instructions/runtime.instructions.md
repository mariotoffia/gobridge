---
applyTo: "runtime/**,bridge/**,adapters/native/cluster/**,adapters/aws/cluster/**,deployment/aws/lib/bootstrap/**"
---

# Runtime, composition root and clustering

Sources: ADR-0001, ADR-0004, ADR-0009, ADR-0012 to ADR-0015, ADR-0017,
`docs/internals/architecture-message-flow.md`,
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

## Lifecycle

- A runtime is single-use. `Start` on a stopped runtime errors; a config
  change builds a new instance. `Terminal()` is true only when both the swap and
  the recovery failed (ADR-0004).
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
