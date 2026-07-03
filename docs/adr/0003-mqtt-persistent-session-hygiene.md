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
(`adapters/mqtt/transport/paho/acl_router.go:54-70`,
`session_reconcile.go`, and the package `doc.go`).

## Decision

Bound the ambiguity with a startup grace window, then reconcile orphans on
evidence, guarding against removing subscriptions the plan still wants.

- **Unmatched-grace window.** For `DefaultUnmatchedGrace = 30 * time.Second`
  (`config.go:190`; a zero value falls back to it, `config.go:81`, `:203`) after
  startup, a publish with no matching binding is buffered — the reconcile may
  still be installing the subscription that covers it.

- **Ack-and-drop past grace.** Once the grace window elapses, an unmatched
  publish is acked and dropped. The topic is recorded as evidence of a live
  orphan subscription so the adapter can act on fact, not on the config diff
  alone.

- **Evidence-based, deduped, exact-topic UNSUBSCRIBE.** The router's grace-worker
  goroutine (`acl_router.go:237`, `:253` `graceLoop`) drains an orphan topic off
  `unsubCh` and invokes the wired `Session.unsubscribeOrphan`
  (`session.go:128`, `session_reconcile.go:90`), which issues a best-effort
  `UNSUBSCRIBE` for the EXACT topic that produced the orphan publish. The dedup
  mark is set when the topic is enqueued (`acl_router.go:308`) and never cleared,
  so each orphan topic is unsubscribed at most once per process.

- **Covered-topic guard.** Before unsubscribing, `unsubscribeOrphan`
  (`session_reconcile.go:90`) calls `topicCoveredLocked`
  (defined at `session_reconcile.go:146`, called at `:94`) to check whether a
  still-desired subscription already covers the topic (an active broker
  subscription or a filter in the current plan). If so, the orphan unsubscribe is
  skipped — removing it would break a wanted subscription.

- **No-op on empty plan.** Reconcile is a no-op when the new plan is empty and a
  prior plan exists (`session_reconcile.go:26`), so a transient empty
  configuration does not unsubscribe topics that are still in use or externally
  managed. Orphan unsubscribes are bounded by `orphanUnsubscribeTimeout` (10s,
  `session_reconcile.go`).

## Consequences

- A message that arrives while its subscription is still being installed is held
  through the grace window instead of dropped. Startup no longer races the
  reconcile.
- An exact-topic orphan subscription is torn down on evidence — after the adapter
  observes a message it cannot route, and only when no wanted subscription covers
  the topic — so it stops stalling the shared session.
- A **wildcard** orphan subscription is never torn down. `UNSUBSCRIBE` matches a
  concrete topic, not a filter, and MQTT exposes no subscription listing, so the
  adapter can only unsubscribe the exact topic a publish arrived on
  (`session_reconcile.go:71-76`). A wildcard orphan survives until process
  restart; its publishes are acked-and-dropped indefinitely (broker and bandwidth
  cost, observable only via the drop metric), but they cannot re-stall the
  session.
- `UNSUBSCRIBE` is best-effort and **not** retried. A `cm.Unsubscribe` failure
  leaves the dedup mark set, so the topic is not re-attempted until process
  restart; the only re-attempt path is the queue-overflow branch
  (`acl_router.go:317-318`), where the enqueue failed and the mark was never set.
  The adapter does not block routing on broker acknowledgment.
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
