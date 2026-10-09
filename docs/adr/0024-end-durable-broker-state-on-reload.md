# 0024 — End a durable subscription's broker state on reload instead of refusing the reload

Status: proposed
Date: 2026-10-09
Deciders: GoBridge core
Amends: [0003](0003-mqtt-persistent-session-hygiene.md) (the managed
subscription history key, and a durable session may now be removed or renamed
by a live reload), [0018](0018-reload-in-place-by-unit.md) (retiring a reload
unit may end the broker state the unit leaves behind)
Relates to: [0011](0011-cluster-client-id-uniqueness.md),
[0013](0013-coordinated-cluster-config-rollout.md),
[0019](0019-dlq-auto-redrive-by-system-event.md),
[0020](0020-contain-unrecoverable-session-by-unit-rebuild.md),
[0022](0022-mqtt-311-by-wire-translation.md)

## Context

### What the broker keeps

Some subscriptions live on the broker, not in GoBridge, and outlive the
connection that made them. The broker files that state under an identity the
broker knows, not under GoBridge's `session_id`:

- **MQTT, Persistent or Exclusive session.** The broker keeps the session's
  subscriptions and queues every matching QoS 1/2 message under the client ID
  until the session expires (MQTT 5 §3.1.2.11.2, §4.1). GoBridge asks for
  86400 seconds by default. On MQTT 3.1.1 the broker decides; Mosquitto never
  expires such a session.
- **AMQP 1.0, durable receiver on a topic address.** The broker keeps a durable
  subscription under the `container_id` and the link name. GoBridge attaches it
  with expiry policy `never` (`receiverLinkOptions` in
  `adapters/amqp/transport/amqp10/acl_session.go`), so it is never removed.
- **Queues (AMQP 0-9-1 queues, SQS, Service Bus).** The queue exists apart from
  any consumer. A consumer that disconnects leaves its unsettled messages in the
  queue, and the next consumer receives them whatever its own identity is. These
  transports have no broker state of this kind.

When GoBridge changes the identity it uses — a new client ID, another broker, a
new `container_id` or link name — or removes the session, it connects as a
different identity, or not at all. The old state stays on the broker:

- the messages the old identity received but had not settled, and the messages
  published while GoBridge was between the two identities, wait there and never
  reach the new identity;
- the broker keeps queuing a copy of every matching message for the old
  identity. That costs broker memory, disk and, on AWS IoT Core, money. On
  ActiveMQ Artemis an address set to `BLOCK` when full stops its producers;
- with MQTT shared subscriptions (`$share`), Mosquitto and EMQX keep handing
  part of the group's traffic to the offline old member until its session
  expires (MQTT 5 §4.8.2 allows it). That traffic is lost to GoBridge.

### What v0.7.1 does about it

v0.7.1 refuses a live reload that changes the identity of a Persistent or
Exclusive MQTT session, or removes or renames one. The Supervisor, the AWS
runtime (through `bridge.ValidateDurableReload`) and the cluster rollout
preflight all run the same comparison. The documented way out is to store the
new configuration and restart the task.

That rule protects nothing:

- **A restart applies the same change.** A starting task has no running
  configuration to compare with, so it connects as the new identity and leaves
  the old state on the broker exactly as a live reload would have.
- **It cannot see a cleanup.** It compares two configurations. An operator who
  did drain and unsubscribe the old session is refused just the same.
- **Nothing ever ends the old state.** No code path deletes a broker session or
  a durable subscription. Refusing the reload only moves the moment the state is
  left behind.
- **AMQP 1.0 is not covered at all**, although its durable subscriptions never
  expire.
- **The keys are mixed up.** The refusal compares per `session_id`, so
  renaming only the GoBridge `session_id` is refused although the broker sees no
  change. The managed subscription history is stored under a fingerprint that
  also covers the session mode, clean start, expiry and protocol version. None
  of those change which broker session the client ID reaches (MQTT 5 §3.1.3.1),
  so changing the expiry files the same broker session's history under a new
  key, and the filters it recorded are never unsubscribed.

## Decision

**A live reload accepts a change to a durable subscription's broker identity,
and the removal of the subscription. Retiring the old reload unit ends the old
identity's state on the broker. Nothing is refused, and no restart is needed.**

### 1. Remove the refusal

The durable session identity comparison is deleted from the Supervisor's apply
path, from `bridge.ValidateDurableReload` (and so from
`bridge.ValidateDormantReactivation` and the AWS runtime's apply), and from the
cluster rollout preflight. The change becomes an ordinary reload: the reload
unit that holds the session is retired and the new one is added
([ADR 0018](0018-reload-in-place-by-unit.md)), serialized as today.

Two things stay:

- **Duplicate identities are still rejected**, as plain configuration
  validation on every configuration: two sessions that would connect as the same
  client ID to the same broker keep disconnecting each other.
- **The other guards in `bridge.ValidateDurableReload` are unchanged**: a
  repointed store, a changed lease `session_id`, and a removed outbox or DLQ
  store. They are not part of this decision.

### 2. The broker state key

A broker state key names one piece of broker-side state that outlives a
connection. Two equal keys are the same state.

| Transport | When it has a key | The key |
|---|---|---|
| MQTT | Persistent or Exclusive session | the canonical broker endpoint and the effective client ID (what `DurableSessionIdentityDomains` computes today) |
| AMQP 1.0 | receiver with `durability_mode` above 0 on a topic (multicast) address | the canonical broker endpoint, the `container_id` and the effective link name |
| everything else | never | — |

The session mode, clean start, session expiry and MQTT protocol version are not
part of the MQTT key; changing them reconnects the same broker session. A
receiver on a queue (anycast) address never has a key, so GoBridge never
deletes a queue.

Keys are computed from the configuration, not from running instances, by an
optional transport factory capability that is given a session and the receivers
bound to it. They have to be known before anything is retired: a serialized
reload retires the old units before it builds the new ones.

A reload compares the keys of the whole running configuration with the keys of
the whole next configuration. A key in the running configuration that is not in
the next one is **lost**. Comparing whole configurations means renaming only a
`session_id` loses nothing, because the broker identity is still there.

### 3. Retiring ends lost broker state

`Runtime.Retire` handles a session that holds a lost key in this order:

1. drain in-flight deliveries, as today;
2. end the broker state;
3. close the session;
4. release its lease.

Only an instance that is connected (MQTT) or attached (AMQP 1.0) at that moment,
and that holds the lease when the session is lease-managed, ends anything. A
cluster standby never connected as the identity and does nothing.

How the state is ended, through an optional capability of the MQTT session and
the AMQP 1.0 receiver:

- **MQTT**: the session disconnects its connection normally, then opens one
  short connection as the old client ID with clean start (on MQTT 5 also
  Session Expiry Interval 0), then disconnects. The broker deletes the session, its
  subscriptions and its queued messages, and removes it from every shared
  subscription group (MQTT 5 §4.8.2). A separate connection is needed because
  an MQTT 3.1.1 DISCONNECT has no Session Expiry Interval to send; on MQTT 5 a
  DISCONNECT could carry one, but one clean-start connection works on both
  versions. On MQTT 3.1.1 clean start is `CleanSession=1`.
- **AMQP 1.0**: the receiver closes its live link instead of dropping the
  connection. go-amqp can only send a closing detach (`detach` with
  `closed=true`, AMQP 1.0 transport §2.6.6), and Artemis and Qpid Broker-J
  delete a durable topic subscription on a closing detach. Today the adapter
  drops the connection precisely to avoid that delete; it still does so for
  every retire that loses no key.

The step is bounded by the transport's connect timeout. When it fails — access
denied, broker unreachable — the session logs a Warn naming `session_id` and
counts the failure in a metric, and the reload continues. The state is then
left as it was before this decision: it expires (MQTT) or stays (AMQP 1.0).

A full replacement (prepare-commit or overlap swap) gives the old runtime's
`Stop` the same set of lost keys.

**Nothing else ends broker state**: not a shutdown, a restart, a pause
(`StopBridge`), a lease loss or failover, the rebuild of a failed session
([ADR 0020](0020-contain-unrecoverable-session-by-unit-rebuild.md)), or the
reload of a unit whose keys all survive.

### 4. MQTT managed subscription history

The history is stored under a SHA-256 digest of the MQTT broker state key, so
the store still holds no client ID or broker URL. Changing the expiry, clean
start, mode or protocol version keeps the history.

**Upgrade.** When no history exists under the new key, the session reads the
history stored under the old fingerprint once and stores it under the new key.
The old fingerprint function remains only for that lookup.

**A key a reload adds.** When a live reload adds a broker state key that the
running configuration did not have — a new client ID, or a new durable session
— the runtime records an empty history for it unless one already exists. A
session that starts with an empty history first ends whatever broker session
its client ID may still have, so the broker holds nothing the history does not
know. An existing history is kept, never emptied: it describes what the broker
may still hold for that client ID, or another cluster member has already filled
it, and the reconcile removes the filters the plan no longer has. At process
start, with no running configuration to compare with, the existing rule is
unchanged: a durable session with no history needs a seeded baseline.

### 5. Corrections that go with it

- The MQTT unsettled-delivery metrics tag `session_id` with the GoBridge
  session ID. They tagged it with the client ID.
- The AMQP 1.0 package documentation names go-amqp v1.7.0, the version the
  module requires. It named v1.5.1.

## Delivery guarantee

- Every delivery the old identity received is settled within `drain_timeout`
  before the state is ended: at-least-once, as on any retire.
- QoS 1/2 messages published between the old disconnect and the new
  subscription's acknowledgement are lost, as on a restart. On AMQP 1.0, so are
  messages sent to the address while neither subscription exists. For `$share`,
  only when the group has no other member.
- Ending the state deletes any backlog the old identity still had. Rolling the
  configuration back does not bring it back.
- For no loss at all, change in two reloads: first add the new session next to
  the old one, then remove the old one. The overlap delivers some messages
  twice, which downstream idempotency absorbs.

## What each broker does

| Broker | Ending the state |
|---|---|
| MQTT 5 and 3.1.1 brokers | The session is deleted, including its shared subscription membership. |
| ActiveMQ Artemis | The subscription queue and its messages are deleted when no other consumer is attached. The user needs the `deleteDurableQueue` permission. |
| Qpid Broker-J | The durable subscription is deleted. |
| Solace | Not deleted: Solace deletes a durable topic endpoint only on a closing detach with a null source, which go-amqp cannot send. The failure is logged. |
| RabbitMQ 4 (AMQP 1.0), Service Bus, SQS | Nothing to end: these addresses are queues or entities provisioned outside GoBridge. |

## Consequences

- Operators can rename a client ID, move a session to another broker, switch
  the MQTT protocol version, change an AMQP 1.0 `container_id` or subscription
  name, and remove a link with a live reload, on a single task and in a cluster
  through the coordinated rollout ([ADR 0013](0013-coordinated-cluster-config-rollout.md)).
- A restart that changes an identity still leaves the old state on the broker,
  because a starting task has no running configuration to compare with. The
  documentation says so and recommends the live reload.
- In a cluster, a member that lags on the old configuration and wins the lease
  after the old state was ended recreates an empty session under the old
  identity. It ends that session when it applies the new configuration. This is
  churn, not loss.
- When an in-place reload fails after retiring and restores the old unit, the
  old identity's broker state has already been ended. The restored session
  starts on an empty broker session, so the backlog the old state held is
  lost, as it would be on a successful reload.
- Dead-letter records written before the upgrade carry the old fingerprint as
  their managed identity, so automatic redrive
  ([ADR 0019](0019-dlq-auto-redrive-by-system-event.md)) does not match them.
  Redrive them by hand.
- The AMQP 1.0 durable subscription delete needs an integration test against
  Artemis; nothing tests it today.
- Stale AMQP 0-9-1 bindings are not covered: the adapter never unbinds a
  routing key it no longer uses, so the old binding keeps routing traffic into
  the queue. That needs its own change.

## Rejected alternatives

- **Keep the refusal and the restart.** It refuses a change that a restart then
  applies, and it ends nothing.
- **Disconnect and reconnect as the new identity, with no ending step.** Correct
  for queues, but on MQTT `$share` groups Mosquitto and EMQX keep routing part of
  the traffic to the offline member, and an AMQP 1.0 durable subscription keeps
  collecting messages forever.
- **A handover inside the running MQTT session**, holding the old and the new
  connection at once. It adds a second connection to the session's connection,
  router and Receive Maximum handling for a rare operator action, it still needs
  the ending step, and the overlap only shortens a loss window that two reloads
  already close.
- **Keys from running instances, ended after the new unit is grafted.** By then
  the lease is released, so another member may have connected as the old
  identity, and the ending connection would take that live connection over. A
  serialized reload also builds the new instances only after the old ones are
  retired.
- **Ending the state only when the configuration asks for it.** Leaving the
  state is never what an operator wants after removing or renaming a link, so
  the default is to end it.
- **Keep the old fingerprint and treat any change to it as a new identity.**
  Changing only the session expiry would then end the session and drop its
  queued messages.
