# 0023 — Keep a resumed MQTT 3.1.1 session's subscriptions

Status: accepted
Date: 2026-10-08
Deciders: GoBridge core
Amends: [0022](0022-mqtt-311-by-wire-translation.md) (retained messages are no
longer replayed on a reconnect the broker resumed; this adds one protocol
version check above the translator)
Relates to: [0003](0003-mqtt-persistent-session-hygiene.md) (the reconcile and
the managed-subscription history that act on the kept subscriptions),
[0021](0021-contain-mqtt-recovery-and-ingress-reject-in-session.md) (a
settlement recovery reconnects through Reload, which this keeps resumable)

## Context

Every connection-up empties the session's record of the subscriptions the
broker confirmed (`observedSubs`, `activeSubs`), and the reconcile the runtime
runs next SUBSCRIBEs every filter of the plan again. On MQTT 5 that costs
nothing: a Persistent or Exclusive session's SUBSCRIBE carries Retain Handling
1, so the broker sends retained messages only for a subscription that did not
exist yet.

MQTT 3.1.1 has no Retain Handling. Every SUBSCRIBE makes the broker send the
filter's retained messages again (MQTT 3.1.1 §3.8.4), so a 3.1.1 session got
every matching retained message again on every reconnect.

A CONNACK with Session Present set means the broker kept the session
(§3.2.2.2), and the session state includes the client's subscriptions
(§3.1.2.4). After such a CONNACK the broker still holds every subscription the
previous connection had confirmed.

## Decision

**On MQTT 3.1.1, a connection-up whose CONNACK has Session Present set keeps
the broker-confirmed subscription record of the connection before it, when the
session is Persistent or Exclusive and all its broker URLs reach one canonical
endpoint.** The reconcile that follows then SUBSCRIBEs only filters that are
new or whose requested QoS changed, and UNSUBSCRIBEs removed ones. An unchanged
filter gets no SUBSCRIBE, so the broker replays nothing for it.

It is a plain branch on the protocol version in
`handleConnectionUpGenerationWithSessionPresent`; `keepResumedSubscriptionsLocked`
and `resetSubscriptionRecordLocked` in `session_resume.go` do the work. Every
other connection-up resets the record exactly as before: MQTT 5, Session
Present not set, and Ephemeral sessions.

Everything else on connection-up is unchanged: `subscriptionsSatisfied` is
false until the reconcile converges the plan, the connection epoch advances,
the unmatched grace window restarts, resume-loss detection runs, and the
QoS-downgrade records stay as they are.

### What a kept record drops

Two kinds of filter are dropped from a kept record, so the reconcile treats
them as after a fresh session:

- **A filter granted below its requested QoS.** Every reconnect still
  re-checks the grant, as on MQTT 5, and the QoS probe keeps acting only on
  the connection whose SUBACK recorded the downgrade.
- **A filter whose last SUBSCRIBE or UNSUBSCRIBE has no recorded
  acknowledgement** (`unackedSubs`). A connection that drops in the middle of
  the round trip leaves the broker's state for that filter unknown. Keeping
  its record could skip the SUBSCRIBE of a filter the broker already removed,
  which would lose it silently, or of one the broker now holds at another QoS.
  Every SUBSCRIBE and UNSUBSCRIBE path marks its filters before sending: the
  reconcile, the managed-subscription cleanup, the orphan cleanup and the QoS
  probe. A mark is cleared only where the acknowledgement is recorded on the
  same connection: `applyGrantLocked`, `removeObservedSubscriptions`, and the
  orphan cleanup's confirmed removal. The orphan cleanup marks whatever the
  connection epoch, because autopaho may send it on a newer connection.

### When the record is dropped

The record assumes that only this session changed the broker session's
subscriptions. It is dropped wherever the session gives the broker session up,
because another owner of the client id may then change them:

- `disconnectGeneration` always resets it, on every protocol version. A failed
  exclusive reconcile calls it and then releases the lease; the terminal
  transition calls it too.
- A Reload that fails resets it on MQTT 3.1.1, before the supervisor can start
  the session again.

A Reload that succeeds keeps the session's ownership: a credential rotation
that needs a new connection, a managed-subscription cleanup, a settlement
recovery. On MQTT 3.1.1 it no longer resets the record at teardown, and leaves
the choice to the replacement's connection-up. MQTT 5 still resets at teardown.

### Which broker Session Present describes

Session Present from another broker would describe that broker's session.
`ValidateSessionMode` already refuses a Persistent or Exclusive session whose
broker URLs reach more than one canonical endpoint. A session built directly
with `NewSession` is not validated that way, so it checks the same condition at
construction (`oneBrokerDomain`). An Ephemeral session connects with Clean
Session set on every connect, so the broker never resumes it.

## Consequences

- A reconnect the broker resumed replays no retained messages. Settlement
  recovery and managed-subscription cleanup reconnect through Reload and
  replay none either.
- Retained messages are still delivered again, marked `mqtt.retained=true`:
  - on a reconnect the broker did not resume, a fresh or lost session
    (`MQTTSessionResumeLost` counts a loss);
  - on the first connection after a process start, because a new session has
    no record, even when the broker resumed the session;
  - on every QoS confirmation and re-check SUBSCRIBE, and on the reconnect of a
    filter granted below its requested QoS;
  - for a filter whose last operation was not acknowledged, and after the
    session gave the broker session up.
- Between connection-up and the reconcile, health lists the kept filters as
  active. `subscriptionsSatisfied` stays false, so the session cannot report
  Full before the reconcile converges.
- The session trusts the broker. A broker that sets Session Present but lost a
  subscription leaves that filter silent until the session next subscribes it.
  MQTT 3.1.1 offers no way to list a session's subscriptions. Only Mosquitto
  is tested (`TestIntegration_MQTT311_ResumedSessionDoesNotReplayRetained`).
- A broker that withdraws read access to a kept subscription stops delivering
  on it without telling the session. A reconnect does not re-check it, just as
  nothing re-checks it on a live connection.
- Two live instances that share a client id can change each other's
  subscriptions. That is already the documented client-id collision; give each
  instance its own `client_id`, or use an exclusive lease.

## Rejected alternatives

- **Drop retained deliveries after a resumed reconnect.** The session could
  re-subscribe every filter as on MQTT 5 and discard deliveries marked RETAIN
  on a resumed connection. The broker would still send every retained message,
  and the session would have to tell a replay from the first retained delivery
  of a filter that is new on that connection. Keeping the record sends nothing
  to discard.
- **Keep the whole record, including filters with an operation in flight.**
  Simpler, but a connection that drops during a reconcile could then leave a
  filter the broker removed marked as subscribed, and the next reconcile would
  never subscribe it again.
- **A strategy or interface per protocol version.** One branch on the protocol
  version at connection-up, and one in Reload, carry the whole difference.
