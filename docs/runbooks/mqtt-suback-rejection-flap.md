# Runbook: MQTT Permanent SUBACK Rejection / QoS Downgrade Flap

**Applies to:** MQTT (paho) receiver sessions, most visibly exclusive
sessions.
**Audience:** on-call operators.
**Risk:** no message loss — the failure is fail-closed by design — but the
affected session (and every route on it) stays down until an operator or a
broker-side change resolves the disagreement. It does **not** self-heal.

## Background

A reconcile fails when the broker rejects ANY requested filter (SUBACK error
reason) or grants a LOWER QoS than requested. GoBridge deliberately has no
per-topic quarantine: serving a partial route set silently would turn a
broker-side policy change into invisible data loss, so the whole reconcile
fails, readiness stays below Full, and — on an **exclusive** session — the
lease is released and the session disconnects. Supervision then retries
forever at the 30s backoff cap: connect → subscribe → reject → disconnect,
indefinitely.

## Symptom

- `ReconcileFailures` climbs steadily; `MQTTQoSDowngraded` counts when
  the cause is a QoS grant below the request.
- Logs repeat the downgrade WARN
  `mqtt: broker downgraded subscription QoS below requested; delivery
  guarantee is weaker than the route assumes` (the propagated error carries
  `mqtt: broker granted subscription QoS below requested`), or a SUBACK
  rejection error naming the `topic`.
- Readiness (`/api/v1/monitor/ready?level=full`) stays 503; for an exclusive
  session the lease changes hands or churns.
- The cycle repeats every ~30s and never converges.

## Diagnosis

1. Get the offending filter from the reconcile failure log (`topic`,
   `requested_qos`, `granted_qos`).
2. Check the broker's ACL / policy for that filter and this session's
   credentials: SUBACK 0x87 (Not authorized) and QoS caps are broker policy,
   not bridge state.
3. Confirm the filter is still wanted in the bridge config (a stale route may
   simply need removal).

## Remediation

Exactly one of:

- **Fix the broker side**: grant the ACL / raise the QoS cap for the filter.
  The next supervised retry converges on its own.
- **Fix the bridge side**: remove or edit the rejected subscription (or lower
  its requested `qos`) in the route config and reload. The reconcile then no
  longer requests the rejected grant.

Do NOT try to "wait it out": the flap is permanent for as long as the broker
and the config disagree.

## Alerting

Alert on `ReconcileFailures` rate sustained for more than ~5 minutes
(three+ consecutive failed retries): transient reconnect reconciles recover
in one or two rounds; a steady rate is this flap.
