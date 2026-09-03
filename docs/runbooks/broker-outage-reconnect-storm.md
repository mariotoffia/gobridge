# Runbook: Broker Outage / Reconnect Storm

**Applies to:** any long-lived transport session (MQTT, AMQP 0-9-1, AMQP 1.0,
Azure Service Bus).
**Audience:** on-call operators.
**Risk:** low to act — the adapters reconnect on their own; the danger is
mistaking a broker outage for a bridge fault and restarting healthy tasks.

## Symptom

- Messages stop flowing, or throughput drops to near zero.
- `MQTTReconnects` / `ReconcileFailures` / `SessionRestarts` climb.
- Logs carry `CONNECTION_LOST`, `UNAVAILABLE`, `THROTTLED`, or `BROKER_BUSY`.
- `GET /api/v1/monitor/health` still returns `{"status":"ok"}` — health does not
  reflect broker connectivity by design, so the pod is not restarted for a
  transient reconnect
  ([health-and-shutdown.md#health-endpoints](../health-and-shutdown.md#health-endpoints)).

## Diagnosis

1. Confirm connectivity separately from health. `/health` stays green through a
   broker outage; gate on the transport instead:

   ```bash
   curl -s "http://<host>:8081/api/v1/monitor/ready?level=connected"
   # 503 → at least one session is not connected to its broker
   ```

   `/api/v1/monitor/deephealth` (authenticated) reports per-session state
   ([http-api-monitor.md](../http-api-monitor.md)).

2. Read the error code to classify the cause. Every rejected CONNECT is counted
   on `MQTTConnectFailures`, tagged `session_id` and the bounded `code`, and the
   same cause is latched on the session's `LastError` until the session comes
   back up — so the reason is visible without catching a log line live
   ([troubleshooting.md](../troubleshooting.md)):
   - `CONNECTION_LOST` — session dropped mid-operation: broker restart, network
     partition, idle-timeout, or a TLS failure on reconnect
     ([troubleshooting.md#connection_lost](../troubleshooting.md#connection_lost)).
   - `UNAVAILABLE` — a dependency declared itself temporarily unable to serve
     ([troubleshooting.md#unavailable](../troubleshooting.md#unavailable)).
   - `THROTTLED` / `BROKER_BUSY` — the broker is rate-limiting or overloaded
     ([troubleshooting.md#broker_busy](../troubleshooting.md#broker_busy)).

3. Watch the reconnect metrics under `GoBridge/Runtime`
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)):
   `MQTTReconnects` (session reconnects), `ReconcileFailures` (reconcile after
   reconnect failed), `SessionRestarts` (supervised per-session restart). In a
   clustered deployment a broker outage that outlasts the lease TTL also raises
   `LeaseExpiries`.

4. If reconnects loop without recovering, check for `MQTTSessionTakeover`
   ([troubleshooting.md#adapter--runtime-diagnostic-metrics](../adapter-diagnostic-metrics.md)):
   two instances sharing one `client_id` kick each other off in a loop.

5. Check whether the outage cost you the durable session. `MQTTSessionResumeLost`
   counts every connection where a persistent or exclusive session asked the
   broker to resume (`clean_start=false`) and the CONNACK answered **Session
   Present=false**. Re-subscribing then succeeds and the session goes green
   again, so this counter — and the matching `LastError` latch, which clears on
   the next converged reconcile — is the ONLY evidence that continuity broke.

6. If reconnects recover but readiness never reaches `full`, look for
   `MQTTQoSDowngraded`. A broker QoS cap fails every reconcile identically; the
   third consecutive confirmation of the same grant is treated as permanent and
   the session goes terminal rather than restarting into it forever.

## Action

- **Do not restart tasks for a reconnecting session.** The adapters reconnect
  automatically (autopaho, the amqp091 reconnect loop, the ASB SDK); a restart
  discards in-flight work and adds load
  ([troubleshooting.md#connection_lost](../troubleshooting.md#connection_lost)).
- **Rejected credentials / duplicate `client_id` / TLS on reconnect** — check
  broker logs. For a duplicate-`client_id` storm (`MQTTSessionTakeover`), give
  each replica a distinct `client_id` or use an exclusive session.
- **`BROKER_BUSY` / `THROTTLED`** — reduce route `max_in_flight` to smooth the
  burst, or enable `shared_outbox` delivery so persistence absorbs it; request a
  broker/SDK quota increase for a chronic throttle
  ([troubleshooting.md#broker_busy](../troubleshooting.md#broker_busy),
  [troubleshooting.md#throttled](../troubleshooting.md#throttled)).
- **`MQTTSessionResumeLost` is non-zero** — the offline QoS 1/2 backlog queued
  for that `client_id` while the session was away is gone, along with its
  broker-side subscriptions; only messages published from the reconnect onward
  are delivered. There is nothing to recover at the bridge — the messages were
  never handed to it. Reduce the exposure for next time: raise
  `session_expiry_interval` above your worst-case outage/failover gap, and
  confirm the broker persists sessions across restarts (an in-memory broker
  loses every session on restart regardless of the interval). If the loss is not
  acceptable, move the source to a store-backed transport rather than relying on
  broker-side offline retention.
- **`MQTTQoSDowngraded` climbing with a terminal session** — the broker refuses
  the QoS the route asks for and the bridge stopped retrying. Either lower the
  route's `qos` to the granted level (accepting the weaker guarantee) or lift the
  broker's QoS cap for this client; the reconcile error names the topic, the
  requested QoS and the granted QoS.
- **Outage exceeds your SLO budget** — escalate to the broker/dependency owner.
  A clustered instance whose lease store also went unreachable steps down and
  eventually goes terminal; the process then exits non-zero so the orchestrator
  restarts it ([health-and-shutdown.md#exit-codes](../health-and-shutdown.md#exit-codes)).

## Related runbooks

- [Lease flapping / split-brain](lease-flapping-split-brain.md) — when the
  outage also disrupts lease coordination.
- [Credential expiry / rotation failure](credential-expiry-rotation-failure.md)
  — when reconnects fail on rejected credentials.
