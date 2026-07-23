# Runbook: Lease Flapping / Split-Brain Suspicion

**Applies to:** clustered deployments with a distributed lease store (DynamoDB or
equivalent).
**Audience:** on-call operators.
**Risk:** low to act — fencing tokens already reject stale writes; the work is
diagnosing why leadership churns.

## Symptom

- Leadership bounces between instances; `LeaseTransfers` / `LeaseExpiries` climb.
- The `Lease Acquire Failures` alarm fires (`LeaseAcquireFailures` > 3)
  ([monitoring.md#cloudwatch-alarms](../aws-deployment/monitoring.md#cloudwatch-alarms)).
- Logs show `STALE_FENCING_TOKEN` or `NO_ROUTE_OWNER`.
- You suspect two instances are both acting as owner (duplicate deliveries).

## Diagnosis

1. Check each instance's role. `/api/v1/monitor/topology` (authenticated) reports
   running state and the compact route list; `/api/v1/monitor/ready` returns the
   failover `role` — `active`, `standby`, or `standalone`
   ([http-api.md#monitor-api-endpoints](../http-api.md#monitor-api-endpoints)).
   Exactly one active owner per exclusive session is correct; two is the
   split-brain you are looking for.

2. Read the lease metrics ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)):
   `LeaseExpiries` (leases lost without renewal — step-down), `LeaseTransfers`
   (re-acquired by this instance — hand-off), `LeaseAcquireFailures`, and the
   `LeaseAcquireLatency` / `LeaseRenewLatency` timers. High renew latency with
   rising expiries points at a slow or unreachable lease store.

3. Confirm fencing is holding the line. `STALE_FENCING_TOKEN` means a guarded
   write (outbox claim/complete, lease renewal, route forward) was **rejected**
   because the caller's `LeaseToken.Version` was older than the current owner's —
   the safety mechanism working, not data corruption
   ([troubleshooting.md#stale_fencing_token](../troubleshooting.md#stale_fencing_token)).
   `NO_ROUTE_OWNER` is the forwarder mid-hand-off and self-heals
   ([troubleshooting.md#no_route_owner](../troubleshooting.md#no_route_owner)).

4. Rule out a shared transport identity. `MQTTSessionTakeover` means two
   instances connected with the same `client_id` and are kicking each other —
   a different failure that looks like flapping
   ([troubleshooting.md#adapter--runtime-diagnostic-metrics](../troubleshooting.md#adapter--runtime-diagnostic-metrics)).

## Action

- **Thrashing leases** (high `STALE_FENCING_TOKEN` / `LeaseTransfers` rate):
  investigate the three documented causes — clock skew between instances,
  an undersized `LeaseTTL`, or a network partition between the instances and the
  `LeaseStore` ([troubleshooting.md#stale_fencing_token](../troubleshooting.md#stale_fencing_token)).
  Verify NTP sync and raise `LeaseTTL` if renewals routinely miss their window.
- **Duplicate `client_id`**: give each replica a distinct `client_id` or move the
  route to an exclusive session.
- **Stuck / stale lease records**: inspect the `LeaseStore` (DynamoDB) directly
  and verify every instance reads the same store
  ([troubleshooting.md#no_route_owner](../troubleshooting.md#no_route_owner)).
- Background and invariants: [ARCHITECTURE.md §16 — Clustered Deployment](../../ARCHITECTURE.md#16-clustered-deployment).

## Standalone multi-replica split brain (no distributed lease store)

The diagnosis above assumes a **distributed** lease store (DynamoDB). A different,
more dangerous split brain occurs when an exclusive route runs with an **in-memory
/ non-distributed** lease store and the deployment is scaled to **more than one
replica**: each process holds its OWN private lease, so every replica believes it
is the sole active owner. There is no fencing between them — they all consume the
same logical traffic in parallel (N-fold duplication) with no `LeaseTransfers` and
no `STALE_FENCING_TOKEN` to signal it, because nothing is shared.

**Detection.** `/api/v1/monitor/ready` reports `role: standalone` (not `active`/
`standby`) on *every* replica, and duplicate downstream deliveries scale with the
replica count. The builder emits a **`SPLIT-BRAIN RISK`** warning at startup when
an exclusive/lease-bearing route is configured without a distributed lease
backend — grep startup logs for it.

**Action.**

- Enforce **`replicas: 1`** for any standalone (in-memory-lease) exclusive
  deployment — a single-instance memory lease is the only safe replica count.
  On Kubernetes pin the Deployment/StatefulSet to `replicas: 1`; on ECS set the
  service desired count to 1.
- To run more than one replica for HA, switch to a **distributed `LeaseStore`
  (DynamoDB)** so exactly one replica holds the lease at a time — then the
  distributed-store diagnosis above applies.
- The `SPLIT-BRAIN RISK` startup warning is not an admission boundary; treat it as
  a hard configuration error in any multi-replica deployment.

## Related runbooks

- [Broker outage / reconnect storm](broker-outage-reconnect-storm.md) — a broker
  or store outage that outlasts the lease TTL triggers step-down.
