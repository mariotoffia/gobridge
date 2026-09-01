# 0003 — MQTT persistent-session subscription hygiene

Status: accepted
Date: 2026-07-03
Deciders: GoBridge core

## Context

The paho MQTT adapter defaults to `clean_start=false` so a shared session
survives reconnects and keeps its subscriptions. Combined with manual acks, that
persistence creates a hazard: a subscription left on the broker from a previous
configuration keeps delivering messages to the session, but the current routing
plan has no binding for that topic. Those messages have nowhere to go.

If the adapter buffers them, an orphan subscription can back-pressure and stall
the shared session for every route on it. If it acks and drops them blindly, it
throws away messages that a still-loading subscription was about to cover. The
adapter cannot tell a genuinely orphaned topic from one whose subscription has
not been reconciled yet.

The design intent is documented in the adapter
(`adapters/mqtt/transport/paho/acl_router.go`,
`session_reconcile.go`, and the package `doc.go`).

## Decision

Bound the ambiguity with a startup grace window, then reconcile orphans on
evidence, guarding against removing subscriptions the plan still wants.

- **Unmatched-grace window.** For `DefaultUnmatchedGrace = 30 * time.Second`
  (`config.go`; a zero value falls back to it, `config.go`) after
  startup, a publish with no matching binding is buffered — the reconcile may
  still be installing the subscription that covers it.

- **Ack-and-drop past grace.** Once the grace window elapses, an unmatched
  publish is acked and dropped. The topic is recorded as evidence of a live
  orphan subscription so the adapter can act on fact, not on the config diff
  alone. **This was later refined: past-grace handling now splits by
  whether the current plan still covers the topic — a covered topic is retained,
  not dropped. See the [2026-07-10 addendum](#addendum-2026-07-10-covered-qos-1-and-2-retention-past-grace).**

- **Evidence-based, deduped, exact-topic UNSUBSCRIBE.** The router's grace-worker
  goroutine (`acl_router.go` `graceLoop`) drains an orphan topic off
  `unsubCh` and invokes the wired `Session.unsubscribeOrphan`
  (`session.go`, `session_reconcile.go`), which issues a best-effort
  `UNSUBSCRIBE` for the EXACT topic that produced the orphan publish. The dedup
  mark is set when the topic is enqueued (`acl_router.go`) and never cleared,
  so each orphan topic is unsubscribed at most once per process.

- **Covered-topic guard.** Before unsubscribing, `unsubscribeOrphan`
  (`session_reconcile.go`) calls `topicCoveredLocked`
  (defined in `session_reconcile.go`) to check whether a
  still-desired subscription already covers the topic (an active broker
  subscription or a filter in the current plan). If so, the orphan unsubscribe is
  skipped — removing it would break a wanted subscription.

- **Empty plan removes managed subscriptions; it is not a blanket no-op.** When
  the new plan is empty and the last successfully applied plan held
  subscriptions, reconcile intentionally UNSUBSCRIBEs every managed subscription
  it established (`session_reconcile.go`;
  `TestReconcile_EmptyPlanRemovesManagedSubs`), so the broker stops delivering
  on stale filters the router would otherwise ack-drop as orphans forever. Only a
  genuinely sender-only transition — an empty plan re-affirming an applied plan
  that itself held no subscriptions, with no broker-observed grants and no managed
  history — is a true no-op. UNSUBSCRIBE removes only the exact filters the
  adapter knows it applied; unknown broker-only filters are not touched. Orphan
  unsubscribes are bounded by `orphanUnsubscribeTimeout` (10s,
  `session_reconcile.go`).

## Consequences

- A message that arrives while its subscription is still being installed is held
  through the grace window instead of dropped. Startup no longer races the
  reconcile.
- An exact-topic orphan subscription is torn down on evidence — after the adapter
  observes a message it cannot route, and only when no wanted subscription covers
  the topic — so it stops stalling the shared session.
- A **wildcard** orphan subscription is never torn down by this path.
  `UNSUBSCRIBE` matches a concrete topic, not a filter, and MQTT exposes no
  subscription listing, so the adapter can only unsubscribe the exact topic a
  publish arrived on (`session_reconcile.go`). Its publishes are
  acked-and-dropped (broker and bandwidth cost, observable via the drop metric)
  without re-stalling the session. A process restart does **not** clear it: with
  the same persistent `client_id` and `clean_start=false`, the restarted process
  RESUMES the same broker session, so the wildcard/shared filter survives, and a
  fresh in-memory `Session` has lost the applied-plan history it would need to
  remove an unknown filter. A wildcard/shared orphan is removed only by a
  successful `UNSUBSCRIBE` of that exact filter, a verified managed-migration
  drain, a clean start / session deletion, session expiry, a **changed** client
  ID (which abandons the old broker state rather than proving it was cleaned up),
  or broker administration. The adapter REPORTS this rather than glossing it: an
  `UNSUBACK` of `0x11` (*no subscription existed*) proves the filter was not the
  concrete topic, so `unsubscribeOrphan` logs a warning naming the remaining work
  — enable managed subscriptions, whose exact durable history converges the
  filter on the next reconcile, or remove it at the broker — instead of the
  Debug-level "unsubscribed orphan topic" it used to log for a removal that never
  happened.
- `UNSUBSCRIBE` is best-effort and **not** retried within the process. A
  `cm.Unsubscribe` failure leaves the dedup mark set, so the exact topic is not
  re-attempted for the life of the process; the only re-attempt path is the
  queue-overflow branch (`acl_router.go`), where the enqueue failed and the mark
  was never set. A restart with the same persistent identity resumes the broker
  session and does not re-issue the failed unsubscribe. The adapter does not block
  routing on broker acknowledgment.
- Operators tuning latency-sensitive startup can shorten `unmatched_grace`, at
  the cost of a higher chance of dropping an early publish for a
  still-reconciling subscription. See the MQTT release note in
  [release-notes.md](../release-notes.md).

## Rejected alternatives

- **`clean_start=true` to avoid orphans entirely.** Discards the persistent
  session, so a reconnect loses in-flight subscriptions and unacked messages.
  Rejected — persistence is the point of a shared durable session.
- **Buffer unmatched publishes indefinitely.** A permanent orphan then grows an
  unbounded buffer and eventually stalls the session. The grace window plus
  ack-and-drop bounds the exposure.
- **Unsubscribe purely from the config diff.** Cannot distinguish an
  externally-managed subscription or an in-flight reconcile from a real orphan,
  and would tear down wanted topics. The covered-topic guard and evidence
  requirement exist to prevent that.

## Addendum 2026-07-10: covered QoS 1 and 2 retention past grace

Provenance: the covered-retention implementation (`MQTTRouterCoveredRetained` and
the covered-topic past-grace split) landed in commit 438139a (2026-07-10) and was
later refactored in commit 4d8d76d (2026-07-13); this addendum text was written in
commit 9d8effb (2026-07-10). The date above is the addendum's commit date.

The original **Ack-and-drop past grace** decision above described an
*unconditional* ack-and-drop once the grace window elapses. That is no longer
accurate. A later hardening split the past-grace path by whether the
current routing plan still **covers** the publish's topic:

- **Covered topic, handler registered late.** The publish is **retained
  un-acked** and redelivered once the handler registers
  (`MQTTRouterCoveredRetained`) — it is **never** acked-and-dropped, so a
  late-registering *live* route cannot lose a QoS 1/2 message.
- **Covered topic, QoS 0 the bounded buffer cannot hold.** Dropped best-effort
  and counted `MQTTRouterCoveredDropped`. QoS 0 carries no redelivery contract,
  so this is the only covered-topic drop.
- **Uncovered topic (genuine orphan).** Acked, dropped, and exact-topic
  UNSUBSCRIBEd exactly as the decision describes
  (`MQTTRouterUnmatchedDropped`).

So "ack-and-drop past grace" now applies only to a topic **no current route
covers**; a covered topic is held, not dropped. This closes the loss window
where an early publish for a still-reconciling QoS 1/2 subscription could be
discarded. The metric semantics are documented on the
[MQTT transport page](../transports/mqtt.md#session-modes) (see the
covered-retention release note) and in
[release-notes.md](../release-notes.md).

**On line numbers.** The `file.go:NN` offsets originally cited in the body of
this ADR have been dropped: they rot as the code moves and were already stale by
this addendum. Cite the named files and symbols (e.g. `graceLoop`,
`unsubscribeOrphan`, `topicCoveredLocked`) as the stable reference. New ADRs
should follow the same rule — files and symbols, not line numbers.

## Addendum: durable exact-filter migration

Persistent/exclusive sessions no longer use concrete delivered topics to infer
wildcard/shared filters. They require a durable managed-subscription ledger and
remove exact historical filters before dispatch. The legacy exact-topic orphan
cleanup described above remains an Ephemeral-session behavior.

A successful UNSUBACK is not sufficient evidence that an unacknowledged shared
QoS 1/2 delivery was redistributed. Brokers may pin it to the persistent
ClientID. GoBridge now retains history through reconnect verification; a matching
replay is held unacknowledged and causes terminal fail-closed migration. Operators
must restore the exact old identity/configuration and handler, drain the replay,
and retry. See the [MQTT transport reference](../transports/mqtt.md#removing-filters-restore-drain-retry)
and [migration runbook](../runbooks/mqtt-managed-subscription-migration.md).
