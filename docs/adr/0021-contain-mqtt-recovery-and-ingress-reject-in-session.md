# 0021 — Contain an MQTT recovery failure and a pre-decode ingress reject in the session

Status: accepted
Date: 2026-10-05
Deciders: GoBridge core
Relates to: [0020](0020-contain-unrecoverable-session-by-unit-rebuild.md) (this
record takes two MQTT faults off the terminal path that 0020 contains with a
unit rebuild; every fault it does not name keeps that path),
[0018](0018-reload-in-place-by-unit.md) (the unit rebuild an exclusive session
can still get is an in-place reload of one reload unit),
[0011](0011-cluster-client-id-uniqueness.md) (the ingress-reject backoff uses
the same stability window as the session-takeover damping and is added to the
reconnect delay the same way)

## Context

ADR 0020 contains an MQTT session that latched the permanent marker
`shared.ErrTransportClosedPermanently`: the composition root rebuilds the reload
unit that holds the session. That is a heavy answer. Every receiver, sender,
binding and route joined to the session stops and is built again, after a
backoff of at least 1 s. ADR 0020 left the transport side of two of its MQTT
faults unchanged and deferred it to a record of its own. This is that record.

1. **A settlement recovery that fails after its drain.** A settlement recovery
   recycles the broker connection of a Persistent or Exclusive session so the
   broker redelivers a delivery that was received but never settled
   ([MQTT settlement recovery](../transports/mqtt-settlement-recovery.md)).
   Before it disconnects, the recovery drains: it stops ingress and waits until
   every delivery the runtime accepted has settled or stopped. Every step after
   the drain could fail the session closed: the reconnect, a CONNACK with
   Session Present = false, a recovery connection that a newer connection
   replaced, and the reconcile that re-subscribes. A broker that dropped the
   connection during a recovery, or that had forgotten the session, was enough
   to rebuild the whole unit.
2. **A pre-decode ingress reject.** The paho session wraps the broker connection
   in a guard that reads each inbound packet before Paho decodes it. The guard
   rejects a packet that is malformed, or that is larger than the Maximum Packet
   Size the client advertised in CONNECT. A compliant broker never sends either.
   The reject latched the session closed. A broker that sends the packet again
   on every resume fails every fresh session the same way, so the ADR 0020
   rebuild would repeat for as long as the broker does it.

Neither fault leaves work that a terminal latch would have to stop. After the
drain finished, every delivery the runtime accepted is settled or stopped. A
rejected packet never reached Paho's decoder, so it never reached a route, and
the connection the reject drops is like any connection a network fault drops,
which never made a session terminal. Keeping old route work from acting after
the session is replaced is the reason a session latches terminal, so neither
fault needs the latch.

## Decision

**Both faults are contained in the paho session. The session does not go
terminal and emits no `SessionError`. ADR 0020 is involved only when an
exclusive session is left with no connection.**

### A settlement recovery that fails after its drain

- A recovery dial always asks the broker to resume the session
  (`clean_start=false`), whatever `clean_start` is set to. When the CONNACK
  answers Session Present = false, the session records the loss exactly as it
  does after an ordinary reconnect: `MQTTSessionResumeLost` counts it, a Warn
  log names it, and the resume-lost error stays on `SessionHealth.LastError`
  until the next successful reconcile clears it. The recovery goes on, and its
  reconcile re-subscribes. While a recovery is requested this holds for every
  connect, even one that this configuration would not otherwise expect to
  resume.
- Any other failure after the drain finished abandons the attempt
  (`abandonRecoveryAttempt`):
  - the reconnect fails;
  - a newer connection replaces the recovery's connection before the recovery
    captures its connection epoch;
  - the recovery's reconcile fails, or it finishes on a connection that replaced
    the recovery's own.
- Abandoning an attempt clears the recovery state and starts the
  settlement-recovery rate limit (`settlementRecoveryMinInterval`): the next
  recovery waits until 30 s after this one ended. The session logs Warn
  `mqtt: settlement recovery abandoned after its drain; the session reconnects normally`.
  It latches no terminal error and emits no `SessionError`.
- What happens next depends on the connection:
  - **A connection is installed.** The session stays connected. The runtime
    session manager runs an ordinary `Reconcile` on it, as it does after any
    reconnect.
  - **No connection is installed, no `Start` is in flight, and the session is
    not closed.** The session closes its events channel. That is the ordinary
    signal of a dead session. The runtime session manager runs a non-exclusive
    session again. An exclusive session whose events close is closed, and the
    supervisor rebuilds its unit (ADR 0020).
  - **A `Start` is in flight.** The `Start` owns the signal. If it fails, its
    caller gets the error and the manager runs the session again. If it
    succeeds, its `SessionConnected` reaches the manager.
- These failures stay terminal, as ADR 0020 describes:
  - a drain that fails;
  - every failure before the attempt reaches its drain: a queued recovery that
    cannot acquire the session gate, a cancelled attempt, and a session that was
    closed or terminal before the attempt began.

  In those cases route work of the old connection may still act, so the session
  latches `shared.ErrTransportClosedPermanently`.

### A pre-decode ingress reject

- When the guard rejects a packet, it:
  1. records the reject on the session;
  2. tries to send an MQTT v5 DISCONNECT with reason code 0x95 (Packet too
     large) for a packet above the advertised Maximum Packet Size, or 0x81
     (Malformed Packet) for a malformed one;
  3. closes the socket. Paho's reader fails, and autopaho reports the
     connection down and reconnects.
- MQTT v5 §3.1.2.11.4 requires the client to disconnect with 0x95 when it
  receives a packet larger than the Maximum Packet Size it advertised. MQTT v5
  §4.13 allows a DISCONNECT with a reason code before the client closes the
  connection on a malformed packet.
- The DISCONNECT is best effort. Paho writes every packet under the guard's
  write lock: the guard is the `sync.Locker` Paho serialises its writes through.
  When a Paho write holds that lock, the guard skips the DISCONNECT and only
  closes the socket, which also unblocks that write; the broker then sees the
  connection drop with no reason code. Otherwise the DISCONNECT write has a 1 s
  deadline.
- **Last Will.** A spec-compliant broker discards the Last Will only after a
  DISCONNECT with reason code 0x00 (MQTT-3.1.2-8), so it publishes the will
  after 0x95 or 0x81. Mosquitto discards the will after every client DISCONNECT
  except 0x04 (Disconnect with Will Message), so on Mosquitto a guard reject
  does not publish the Last Will. That practically never happens there:
  Mosquitto enforces the client's Maximum Packet Size itself and does not send
  a larger packet.
- The session records each reject:
  - `MQTTIngressRejected` counts it, tagged `session_id`.
    `MQTTRouterDropped` does not count it;
  - an Error log names the cause and the streak, the number of consecutive
    rejects;
  - the reject is on `SessionHealth.LastError`.
- **Reconnect backoff.** After any connection that came up, autopaho starts its
  attempt count again, and its first attempt has no delay. A broker that sends
  the packet again on every resume would be redialled in a tight loop. While the
  last reject is less than 30 s old, each reconnect therefore waits an extra
  delay keyed on the streak:
  - the step is `reconnect_delay` × 2^(streak − 1), capped at
    `reconnect_max_delay`;
  - the wait lies between half and all of the step (equal jitter).

  The delay is added to autopaho's own backoff, the same way the
  session-takeover penalty is (ADR 0011).
- **Health.** While the reject is active, the session reports `Ready` false and
  `ServiceLevel` None, and `LastError` includes the reject. The reject stays
  active until a connection that came up after the last reject has stayed up
  for 30 s (`connectionStabilityWindow`, which the takeover damping also uses).
  A session therefore reads not ready for at least 30 s after a reject. Once a
  connection has held that long, the next reject starts a new streak, and the
  reject is forgotten at the next connection-down or planned teardown: a
  `Reload`, a disconnect, or `Close`.

## Safety

ADR 0020 relies on two guarantees. Both still hold.

1. **No new broker connection while old route work can still act.** The
   guarantee covers the connection a recovery recycle or a replacing session
   opens, not the reconnect autopaho makes inside the same session after a
   network fault. The recovery change covers only what happens after the drain
   finished. At that point every delivery the runtime accepted from the old
   connection is settled or stopped, so no route work of the old connection
   exists. A failed drain, and every failure before the drain, still latch the
   session terminal under ADR 0020. A pre-decode reject adds no route work: the
   rejected packet never reached a route. No session replaces the one that
   rejected it; the connection the reject drops is the ordinary one autopaho
   reconnects, and the deliveries still held from it settle as they do after
   any reconnect.
2. **No lease release while old work can still act.** A failed drain still
   latches the session terminal, and the lease rules of ADR 0020 apply. An
   abandoned recovery closes the events channel only after a completed drain.
   When an exclusive session manager then closes the session and releases its
   lease, the old work has already stopped. A pre-decode reject does not end
   the session: the session manager sees an ordinary disconnect and reconnect,
   and the lease follows the rules for any broker outage.

## Consequences

- A broker that drops the connection during a settlement recovery, or that has
  forgotten the session, no longer rebuilds the reload unit. The session
  reconnects. A forgotten session is reported as a resume loss
  (`MQTTSessionResumeLost`), as on an ordinary reconnect. The broker-side
  backlog for the `client_id`, including the delivery the recovery wanted
  redelivered, is gone with the broker session. A rebuild could not bring it
  back either.
- An abandoned recovery starts the 30 s rate-limit cooldown, so a broker that
  keeps failing recoveries gets at most one recovery attempt per session every
  30 s.
- An exclusive session left with no connection after an abandoned recovery
  closes its events channel, so it still gets an ADR 0020 unit rebuild. The
  rebuild starts after the drain, when no old work can act.
- A broker that sends a malformed or oversized packet now cycles the connection,
  not the unit. Each reject counts `MQTTIngressRejected`, logs an Error, and
  reconnects with a backoff that grows with the streak up to
  `reconnect_max_delay`. A compliant broker never sends such a packet, so alert
  on any non-zero `MQTTIngressRejected`
  ([MQTT ingress poison](../runbooks/mqtt-ingress-poison.md)).
- After a reject the session reads not ready for at least 30 s, and for as long
  as rejects keep arriving.
- On Mosquitto a guard reject does not publish the Last Will. A deployment that
  relies on the will to announce that the bridge went offline gets no
  announcement for a guard reject on Mosquitto. A spec-compliant broker
  publishes the will.
- The known risk ADR 0020 names for a broker that keeps sending a malformed
  packet — a unit rebuilt again and again — no longer applies to that fault.

## Rejected alternatives

- **Keep both faults terminal and rely on the ADR 0020 rebuild.** It answers a
  broker blip with a restart of the whole unit: every route joined to the
  session stops and is built again. A pre-decode reject that the broker sends
  again on every resume would fail every fresh session the same way, so the
  unit would be rebuilt forever.
- **Ack and drop a malformed or oversized packet, as the publish callback does
  for a local-cap violation.** The guard runs below Paho, where there is no way
  to ack a packet. The length of a malformed packet cannot be trusted, so the
  guard cannot find the next packet boundary to skip it safely.
- **Retry the recovery in place without a cooldown.** A broker that keeps failing
  would be hammered with recycles. The abandoned attempt starts the rate-limit
  cooldown instead.
- **Send DISCONNECT 0x04 (Disconnect with Will Message) so Mosquitto keeps the
  Last Will.** It drops the reason code that MQTT v5 §3.1.2.11.4 requires for a
  packet that is too large.
