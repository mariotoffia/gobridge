# 0019 — DLQ automatic redrive triggered by system events

Status: accepted
Date: 2026-09-24
Deciders: GoBridge core
Amends: [0017](0017-direct-hold-in-process-send-retry.md) (its redrive-batch
consequence: each entry of an admin redrive batch now has its own bound)
Relates to: [0015](0015-dlq-redrive-inject-then-delete.md) (every rule of an
admin redrive also holds for an automatic one),
[0003](0003-mqtt-persistent-session-hygiene.md) (the removed-subscription
dead-letter this redrives)

## Context

Some dead-letter records wait only for a condition the bridge can see for
itself. The first case comes from the ADR 0003 addendum. When a configuration
change removes a filter from a persistent or exclusive MQTT session, the broker
can still hand the session deliveries for that filter. With a dead-letter store,
the session writes each one to the store with error code `SUBSCRIPTION_REMOVED`
and acknowledges it. A filter is often removed by mistake, or as one step of a
staged migration, and then restored: the operator rolls the configuration back
or enables the filter again. Until now the operator then had to find those
records and redrive each one through the admin API, although the bridge already
knows the filter is back.

A second problem sat in the admin redrive itself. One 30-second bound covered a
whole redrive batch, and entries are redriven one after another. Since ADR 0017
a `direct_hold` route retries a failing send inside the bridge for its
`send_retry_budget` (60 s by default). Against a destination that was down, the
first entry of a batch could use all 30 seconds, and every later id came back
`redrive deadline exceeded before entry lookup` without being tried.

## Decision

### The record says whether automation may redrive it

- `DLQEntry` gains two fields. `RedriveMode` (`redrive_mode`) is `""` (manual:
  only an operator redrives the record; the default, and what every older
  record reads back as) or `"auto"` (a matching system event may redrive it).
  `ExtraInfo` (`extra_info`) is a map of string facts that such an event must
  match. The names avoid "action", which `FailureAction`, `ExpiredAction`,
  `FilteredAction` and `ports.AuditEvent.Action` already use.
- The mark says what the record is; the runtime decides whether to act on it. A
  record is marked `auto` even when automatic redrive is turned off.
- Every DLQ store keeps both fields. The memory store copies them. The SQLite
  store has two new columns and adds them to an existing file the first time
  it opens it; it only adds columns, never drops or rewrites one, and two
  processes upgrading the same file at once both succeed. The DynamoDB store
  writes two optional string attributes, left out when empty; its table and
  indexes do not change.
- The admin API shows both fields on every DLQ entry. `extra_info` is always a
  JSON object: `{}` when the record has no facts, never `null`.

### The first trigger and the record it redrives

- A rule pairs a trigger with the record type (`ErrorCode`) it may redrive and
  the `ExtraInfo` keys the event and the record must agree on. There is one
  rule: the trigger `subscription_added` redrives `SUBSCRIPTION_REMOVED`
  records, matching on `session_id`, `managed_identity` and `subscription`.
  Another pair is another rule; the mechanism stays the same.
- The removed-subscription dead-letter is written with `redrive_mode: auto` and
  those three facts when the session reports its managed subscription identity:
  the opaque fingerprint of the broker-side session that its durable filter
  history is stored under. `subscription` is the removed filter exactly as
  configured, for example `$share/group/sensors/#`. Without an identity the
  record is manual, as before.
- A record matches when it is `auto`, its `ErrorCode` is the rule's, and every
  key of the rule has the same non-empty value in the record and in the event.
  A missing fact never matches.

### The trigger is a session hook

- Two optional session capabilities are added in `ports/session_hooks.go`:
  `ManagedSubscriptionIdentityReporter` and `SubscriptionAddedHookConfigurer`.
  The MQTT (paho) session implements both.
- The session calls the hook after the broker grants a SUBSCRIBE for filters
  that were not in its managed subscription history when a reconcile first saw
  them. A grant below the requested QoS counts, because the broker holds the
  subscription. A filter subscribed again after a reconnect or a restart is
  already in the history, so it does not fire. A SUBSCRIBE that fails keeps the
  mark, and the retry that succeeds reports the filter once. The session calls
  the hook outside its lock, and the hook must return at once.
- The runtime installs the hook in `Start` and in `Graft`, before session
  goroutines begin, on every session that implements both capabilities and
  reports a non-empty identity. It installs nothing without a DLQ store or with
  the window turned off.
- The hook never takes the runtime lock, because the session calls it from
  inside a reconcile. It starts one background goroutine for the event and
  returns. That goroutine never reports an error, so a failed automatic redrive
  cannot make the runtime terminal. `Stop` waits for it.

### One pass

1. **Wait until the runtime can deliver.** Every second, on the runtime clock,
   the pass checks that the runtime is at least at readiness level
   `subscribed` and that the session's single ingress route has a started route
   runner. When the runtime is ready but the session has no single ingress route
   (none, or several), the pass logs a warning and ends; the records stay for an
   operator. A shutdown ends the wait.
2. **One pass at a time.** A lock is held for the whole pass (not for the wait),
   so two events cannot redrive one record twice.
3. **List.** The pass lists that route's records that failed inside the window,
   `stores.dlq.auto_redrive_window`: 24 hours by default, and `0s` turns
   automatic redrive off. It lists only records that failed before the pass
   started or in its first millisecond, so a route that keeps dead-lettering
   during the pass cannot keep it paging; those records wait for the next
   event. The bound is exclusive and stores keep `failed_at` to the
   millisecond, so it sits one millisecond past the start: a record written in
   the pass's own millisecond is still included. It reads pages of 100,
   oldest first, paging forward from the last `failed_at` it saw and skipping
   records it has already seen. It stops on a short page or on a page with
   nothing new. Each list call has its own 30-second bound.
4. **Redrive each match, oldest first**, inject-then-delete with the rules of
   ADR 0015: a fresh envelope ID with a causation link to the original, the
   replay confined to the record's binding, and a delete only after a confirmed
   inject. The inject runs under the runtime's work context, so a shutdown
   abandons it and the record stays. The delete after a confirmed inject is not
   cancelled by a shutdown and has a 30-second bound; skipping it would redrive
   the message a second time. A failed delete is logged and recorded in the
   audit detail as `delete_error`; the record stays, and a later event may
   redrive it again (at-least-once).
5. **Audit and count.** Each record is audited as `dlq.redrive.auto`: the actor
   is the runtime's instance ID, the resource is `dlq`, the resource ID is the
   record ID, the outcome is `success` or `failure`, and the detail holds
   `route_id`, `session_id`, `subscription`, and `error` or `delete_error`. The
   counters `DLQRedrives` and `DLQRedriveFailures` move with the `route_id` tag,
   as for an admin redrive.

### No second record for a temporary failure

An admin redrive injects a synthetic delivery whose `Retry` answers "not
supported". So a recoverable failure that the route would hand back to its
source becomes a new `retry_unsupported` dead-letter record. On the automatic
path that would add a record on every failed attempt. The automatic inject sets
a hold option instead: `Retry` returns an error, the route returns that error
without writing a record, and the original record stays the only one,
unchanged.

### When a redrive fails

- The record keeps its mode and its facts, so the next matching event tries
  again.
- A failure that wraps `ports.ErrInjectNotDelivered` means the route settled
  this one message without delivering it: dropped, filtered, expired or
  dead-lettered. The pass moves on to the next record.
- Any other failure stops the pass: a held temporary failure, a missing route or
  binding, a runtime that is stopping or fenced. The destination is probably
  down, and trying the remaining records would only repeat the failure.
- A panic during the inject (a sender bug, say) is recovered by the pass and
  handled as such a failure, logged at error level with the panic value. It
  does not make the runtime terminal.

### Clustering

- The hook fires only in the process where the session runs. For an exclusive
  session that is the lease owner.
- The members of a cluster share one DLQ store. `managed_identity` names one
  broker-side session, so a member whose persistent session has its own client
  ID, and so its own identity, never redrives another member's records.

### Admin redrive: a bound per entry

- The admin batch keeps its 30-second bound, detached from the request. Each
  entry's lookup and inject now also have a 10-second bound, taken from the
  batch bound, so the effective bound is the smaller of 10 seconds and what is
  left of the batch. A slow first entry fails on its own, and the later entries
  are still attempted.
- The delete after a confirmed inject runs under the batch bound only, so it
  cannot lose a race with the entry bound.
- A lookup that ends because a bound ran out reports `redrive deadline exceeded
  before entry lookup`, not `entry not found`. An entry the batch never reaches
  reports that label, or, on a store whose lookup ignores its context (the
  in-memory store), an `inject failed: …` error that names the context
  deadline.
- The automatic pass needs no bound per record. It stops at the first failure
  that is not about one message, so a destination that is down costs one
  attempt. Once an inject holds an in-flight slot on the route, the route's own
  budgets bound it (`send_timeout`, `send_retry_budget`, `processor_timeout`).
  The wait for that slot is bounded only by the pass's context, the runtime's
  work context, which has no deadline: behind a full route the pass waits
  until a slot frees or the runtime stops.

## Consequences

- Adding a removed filter back on the same broker session redrives its
  dead-letters that are younger than the window, with no operator action. The
  runbook `docs/runbooks/mqtt-managed-subscription-migration.md` says what an
  operator still does by hand.
- Known limits. None of them loses a message: a record the pass does not
  redrive stays in the store for a manual redrive, and a duplicate is the
  at-least-once duplicate ADR 0015 already accepts.
  - Records filed under a route that was since renamed, or with an empty route
    (no single ingress route when they were written), are never listed: the
    pass lists by the current route.
  - A permanent failure during an automatic redrive is dead-lettered by the
    route as usual, as a new manual record under the redriven message's fresh
    envelope ID. The original record stays too, so one message shows two
    records.
  - A SUBSCRIBE that fails, followed by a process restart before it succeeds,
    loses the trigger: the history is written before the SUBSCRIBE, so the
    restarted session sees the filter as known.
  - After an UNSUBACK with reason `0x11` (no subscription existed) the
    removed-subscription dead-letter can succeed and the history `Forget` then
    fail. The filter stays in the managed history, so adding it back before a
    successful retry does not fire the trigger.
  - "One redrive per record across two events" relies on the DLQ store's read
    consistency. On DynamoDB the route index is eventually consistent, so the
    same filter reported twice inside that lag can redrive a record twice.
  - The pass lock serializes automatic passes only. An admin redrive of the
    same record at the same time is not serialized with a pass, so the record
    can be redriven twice.
  - 100 or more records that were not redriven and share one `failed_at`
    millisecond stop the paging for that pass: a full page of records already
    seen gives nothing new. Later records wait for the next event.
  - A pass that waits for readiness holds one goroutine until the runtime is
    ready or stops.
  - A reload of the session's unit while a pass waits or runs can end that
    pass: between Retire and Graft the session has no single ingress route, so
    a waiting pass gives up, and a running inject fails and stops the pass. The
    re-grafted session does not fire the trigger again, because the filter is
    already in its managed history. The records stay for a manual redrive.
- A change of `auto_redrive_window` is a `stores` change, so a reload replaces
  the whole runtime rather than reloading in place (ADR 0018 reloads only
  sessions, receivers, senders, bindings and routes in place). The validator
  rejects a malformed or negative window, and the key on any store role other
  than `dlq`. A document without the key keeps its content identity, because
  the normal form does not fill in the default.
- A program that builds a runtime without `runtime.WithAutoRedriveWindow` gets
  the 24-hour default. `ports.StoreConfig.AutoRedriveWindowDuration` also gives
  the default for a malformed or negative value that was never validated.
- During an outage each admin redrive entry can use its full 10 seconds, so a
  batch reaches about three entries in its 30 seconds; the rest report the
  deadline label and stay in the store.

## Rejected alternatives

- **A new `SessionEvent` kind for the trigger.** The session event channel may
  drop its oldest unread event under a storm, which would lose a trigger
  without a trace, and the session manager loops would need a change to forward
  the event to the runtime. A hook, installed the way
  `RemovedSubscriptionDeadLetterConfigurer` is, cannot be dropped and needs no
  change to those loops.
- **Listing without the route index**, to reach records under a renamed route
  or with an empty route. That is a scan of the whole DLQ on every event.
- **A deadline for the whole automatic pass.** It would cut off a long pass into
  a healthy destination, and against a destination that is down the pass
  already stops at its first failure.
