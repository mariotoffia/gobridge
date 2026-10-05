# Runbook: MQTT Ingress Poison / Malformed or Oversized Broker Packet

**Applies to:** MQTT (paho) transport sessions.
**Audience:** on-call operators.
**Risk:** for a cap-violating publish, each poison drop is an **acknowledged,
deliberate message loss** — the bridge acks a publish it refuses to process.
The session itself stays healthy; the urgency is finding the publisher, not
saving the bridge. A malformed or oversized packet from the broker is
different: nothing is acked, but the session drops its connection and reads
not ready for at least 30 s after each reject; see
[Broker sends a malformed or oversized packet](#broker-sends-a-malformed-or-oversized-packet).

## Background

The MQTT CONNECT advertises one inbound limit the broker can enforce: the
whole-packet Maximum Packet Size (`max_payload_bytes` + a 128 KiB metadata
allowance). The bridge's finer caps — `max_payload_bytes` itself, the ingress
metadata byte cap (128 KiB), and the User Property count cap (128) — are
**local**: a compliant broker forwards any packet whose total fits, even when
an individual cap is violated. Any authorized publisher can therefore produce
such a packet, accidentally or deliberately.

The bridge **acks-and-drops** these packets (`MQTTIngressPoisonDropped`,
Error log once per violation class, Debug for repeats). It must not do
anything else: an un-acked rejection is redelivered by the broker on every
`clean_start=false` resume, and a session-terminal rejection would loop
restart → redeliver → terminal forever — a permanent, publisher-triggerable
kill switch for every route on the session.

Two violation classes are handled earlier, at the raw pre-decode guard,
because only a **broken broker** can produce them: malformed MQTT structure and
a packet larger than the advertised Maximum Packet Size. See
[Broker sends a malformed or oversized packet](#broker-sends-a-malformed-or-oversized-packet).

## Symptom

- `MQTTIngressPoisonDropped` is non-zero (tagged `session_id`).
- One Error log per violation class:
  `mqtt: acked-and-dropped inbound packet violating a local ingress cap ...`
  with `class` = `payload` | `user_properties` | `metadata`, plus `topic` and
  `payload_bytes`.
- The session stays connected and `ready`; traffic on other topics flows.
- `MQTTIngressUserPropertiesTruncated` is non-zero alongside `user_properties`
  drops: the publisher sent more than 129 User Properties per packet and the
  pre-decode guard cut the list to 129 before the SDK decoded it, so the Error
  log's count reads 129 whatever was sent. The count on the wire is in the
  session's Debug log (`wire_user_properties`).

## Diagnosis

1. Read the Error log for the violation `class`, `topic`, and sizes. Repeated
   drops of the same class log at Debug — raise the log level temporarily if
   you need per-message evidence, and for the real User Property count of a
   truncated packet.
2. Identify the publisher from the topic and broker-side logs/ACLs. The
   bridge cannot name the publisher — MQTT carries no producer identity.
3. Decide whether the traffic is legitimate:
   - **Legitimate but over-cap** (e.g. a producer legitimately sends 300 KiB
     payloads against a 256 KiB `max_payload_bytes`): raise the cap.
   - **Producer bug** (runaway header count, unbounded metadata): fix the
     producer.
   - **Hostile** (deliberate cap probing): revoke the publisher's broker ACL.

## Remediation

- **Raise the cap** when the traffic is wanted: `options.session.max_payload_bytes`
  (payload class). The metadata (128 KiB) and User Property (128) caps are
  fixed adapter constants; traffic that exceeds them needs
  a producer-side fix, not a bridge knob.
- **Fix or block the publisher** otherwise (broker ACL / credential
  revocation).
- **No bridge restart is needed** in either case — the drops are per-packet
  and the session is healthy. Messages already dropped are gone (they were
  acked); if their content matters, the producer must resend within the caps.

## Alerting

Alert on ANY non-zero `MQTTIngressPoisonDropped` rate: it is always either a
misconfigured cap, a broken producer, or hostile traffic — never steady-state
normal.

## Broker sends a malformed or oversized packet

The raw pre-decode guard rejects a packet with a malformed MQTT structure, or
one larger than the Maximum Packet Size the bridge advertised in CONNECT
(`max_payload_bytes` + 128 KiB), before Paho decodes it. The packet never
reaches a route, and nothing is acked. The guard drops the connection, not the
session: the session reconnects and is never terminal
([ADR 0021](../adr/0021-contain-mqtt-recovery-and-ingress-reject-in-session.md)).

### Symptom

- `MQTTIngressRejected` advances, tagged `session_id`. `MQTTRouterDropped` does
  not count these rejects.
- An Error log per reject:
  `mqtt: rejected inbound packet before Paho decoding; dropping the connection`,
  with `client_id`, `error` and `streak` (the number of consecutive rejects).
  The `error` is `mqtt: inbound packet size N exceeds Maximum Packet Size M before decoding`
  or `mqtt: malformed inbound packet rejected before decoding`.
- The session reconnects, and each reconnect waits longer while rejects keep
  arriving: an extra delay that starts at `reconnect_delay` and doubles with
  each consecutive reject, up to `reconnect_max_delay`, with jitter. The extra
  delay stops once no reject has arrived for 30 s.
- Deep health reports the session `ready: false` with `service_level: none`,
  even while `connected` is true. It stays that way until a connection that
  came up after the last reject has stayed up for 30 s, so expect at least 30 s
  of not-ready after every reject.
- A broker that logs DISCONNECT reason codes shows the client disconnecting
  with 0x95 (Packet too large) or 0x81 (Malformed Packet). Mosquitto does not
  log the code: it logs `Received DISCONNECT from <client id>` at debug level
  and `Client <client id> disconnected.` The DISCONNECT is best effort: when
  the bridge was writing a packet at that moment, it closes the socket without
  one, and the broker logs a plain connection drop.
- **Last Will.** A spec-compliant broker publishes the session's Last Will
  after 0x95 or 0x81, and after a drop with no DISCONNECT. Mosquitto does not
  publish it when the DISCONNECT reaches it: it discards the will after any
  client DISCONNECT except 0x04. It does publish the will when the bridge
  closes the socket without a DISCONNECT. Mosquitto enforces the client's
  Maximum Packet Size itself, so a reject practically never happens there.

### Diagnosis

1. Read the Error log. The packet size and the limit tell an oversized packet
   from a malformed one.
2. Find what sent the packet. A compliant broker never forwards a packet above
   the client's Maximum Packet Size and never sends malformed MQTT, so the
   cause is the broker or something between the broker and the bridge: a
   proxy, a load balancer, a WebSocket gateway.

### Remediation

- Fix or replace that component. No bridge-side setting is the fix.
- The session recovers on its own once the packets stop. A broker that sends
  the same packet again on every resume keeps the session cycling —
  reconnecting, rejecting, waiting out the backoff — and not ready, but the
  other sessions and routes in the process keep running.
- Do not restart the bridge for it: a restart does not stop the broker from
  sending the packet.

### Alerting

Alert on ANY non-zero `MQTTIngressRejected`: a compliant broker never sends the
packets it counts.
