# Runbook: MQTT Permanent SUBACK Rejection Flap / QoS Downgrade

**Applies to:** MQTT (paho) receiver sessions, most visibly exclusive
sessions.
**Audience:** on-call operators.
**Risk:** no message loss — the failure is fail-closed by design — but the
affected session (and every route on it) stays down until an operator or a
broker-side change resolves the disagreement. It does **not** self-heal.

## Background

A reconcile fails when the broker rejects ANY requested filter (SUBACK reason
code `0x80` or higher). GoBridge deliberately has no per-topic quarantine:
serving a partial route set silently would turn a broker-side policy change
into invisible data loss, so the whole reconcile fails, readiness stays below
Full, and — on an **exclusive** session — the lease is released and the session
disconnects. Supervision then retries that session on its own, forever, at the
30s backoff cap: connect → subscribe → reject → disconnect, indefinitely. Other
sessions are not affected.

A broker that grants a LOWER QoS than requested does **not** cause this flap;
see [A QoS downgrade is not a flap](#a-qos-downgrade-is-not-a-flap).

## Symptom

- `ReconcileFailures` climbs steadily.
- Logs repeat a SUBACK rejection error naming the `topic`.
- Readiness (`/api/v1/monitor/ready?level=full`) stays 503; for an exclusive
  session the lease changes hands or churns.
- The cycle repeats every ~30s and never converges.

## Diagnosis

1. Get the offending filter from the reconcile failure log (`topic`).
2. Check the broker's ACL / policy for that filter and this session's
   credentials: SUBACK 0x87 (Not authorized) and QoS caps are broker policy,
   not bridge state.
3. Confirm the filter is still wanted in the bridge config (a stale route may
   simply need removal).

## Remediation

Exactly one of:

- **Fix the broker side**: grant the ACL for the filter. The next supervised
  retry converges on its own.
- **Fix the bridge side**: remove or edit the rejected subscription in the
  route config and reload. The reconcile then no longer requests the rejected
  filter.

Do NOT try to "wait it out": the flap is permanent for as long as the broker
and the config disagree.

## Alerting

Alert on `ReconcileFailures` rate sustained for more than ~5 minutes
(three+ consecutive failed retries): transient reconnect reconciles recover
in one or two rounds; a steady rate is this flap.

## A QoS downgrade is not a flap

When the broker grants a subscription a lower QoS than requested (for example
a Mosquitto `max_qos` cap), the reconcile succeeds. The session confirms the
grant with three fresh SUBSCRIBEs 5 s apart, then keeps the subscription active
at the granted QoS as best effort. `MQTTQoSDowngraded` counts the first report,
the Error log names `topic`, `requested_qos` and `granted_qos`, and the gauge
`MQTTQoSDowngradedActive` stays above zero while the downgrade stands. A lower
grant never stops the session or the process.

To remove it, lower the route's `qos` to the granted level, or lift the
broker's QoS cap. The session re-checks the grant every `qos_recheck_interval`
and on every reconnect. See
[QoS downgrade](../transports/mqtt-behavior.md#qos-downgrade).
