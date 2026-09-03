# Runbook: MQTT Ingress Poison (Cap-Violating Publishes)

**Applies to:** MQTT (paho) transport sessions.
**Audience:** on-call operators.
**Risk:** each poison drop is an **acknowledged, deliberate message loss** —
the bridge acks a publish it refuses to process. The session itself stays
healthy; the urgency is finding the publisher, not saving the bridge.

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

Two violation classes remain session-terminal because only a **broken broker**
can produce them: malformed MQTT structure and totals above the advertised
Maximum Packet Size. Those reject at the raw pre-decode guard and fail the
session closed; if a session restart-loops with
`mqtt: inbound packet size ... exceeds Maximum Packet Size` or
`malformed inbound packet rejected before decoding`, the broker is
non-compliant — fix or replace the broker; no bridge-side setting clears it.

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
  (payload class). The metadata and User Property caps are fixed adapter
  constants sized to the ingress memory model; traffic that exceeds them needs
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
