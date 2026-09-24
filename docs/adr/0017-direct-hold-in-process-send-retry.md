# 0017 — `direct_hold` retries a failed send in process before replay or dead-lettering

Status: accepted
Date: 2026-09-22
Deciders: GoBridge core
Relates to: 0015 (a redrive whose inject fails only temporarily is dead-lettered
again less often), 0004 (a wedged route stops the loop)
Amended by: [0019](0019-dlq-auto-redrive-by-system-event.md) (each entry of an
admin redrive batch has its own bound)

## Context

A `direct_hold` route holds the source message unacknowledged while it sends to
the destination, and settles the source only once the send has returned. When
that send failed with a recoverable error, the route never tried the send again
itself. One failed send led straight to one decision:

- hand the message back to its source (`Delivery.Retry`), so the source
  redelivers it later; or
- treat it as permanently failed and write it to the dead-letter store.

Which of the two applied depended on whether the bridge could count how many
times it had already seen that message:

- **A message with a stable identity** — a producer-supplied `mqtt.message-id`,
  MQTT correlation data, or a bridge dedup/idempotency key — was handed back,
  up to `max_replay_attempts` (default 5). On MQTT, handing a message back
  means recycling the whole session: a disconnect and a reconnect, which
  redelivers every unsettled delivery on that session and interrupts every
  subscription on it.
- **A message the bridge cannot count** — an ordinary MQTT publish with no
  producer id, for which the adapter generates a fresh envelope id on every
  arrival — could not be counted across redeliveries, so it was written to the
  dead-letter store on its **first** failure, under the category
  `unstable_identity`.

What operators saw was that a destination which was unavailable for a few
seconds — a queue policy still propagating, a broker restart, a throttling
burst — either emptied into the dead-letter store, one manual redrive per
message, or recycled the MQTT session over and over.

Nothing else covered that gap. The SQS sender inherits the AWS SDK's own
retryer, which absorbs a few seconds; the MQTT and AMQP senders have no such
layer at all.

The route setting `replay_budget` (default 15 minutes) reads like a retry
budget for these routes. It is not one: only the `shared_outbox` drainer reads
it, and a `direct_hold` route never has.

## Decision

**A `direct_hold` route retries a recoverable send inside the bridge, with
backoff, for a bounded time, before the decision above runs.** The source
message stays held and unacknowledged for the whole time, so nothing is
acknowledged that was not delivered and nothing is dead-lettered that a short
outage would have cured.

**The loop.** Each pass is one physical send under its own `send_timeout`, and
keeps the per-send wedge ceiling it always had. Between two sends the route
waits on the same backoff ladder it uses everywhere else: a `RetryAfter` hint
from the destination (SQS throttling, for example) replaces the backoff step
and is not jittered, and otherwise the route's `backoff` applies — 1 s doubling
to 30 s with jitter, by default. Either way the wait is never shorter than the
100 ms floor described next. With the default backoff and no hint, the first
wait is therefore about one second.

**No wait is shorter than 100 ms.** A shorter backoff interval or `RetryAfter`
hint is raised to that floor before the budget is asked whether it can cover
the wait, so a budget too short for even the raised first wait is spent at once.
The floor caps one delivery at budget ÷ 100 ms sends — about 600 at the
60-second default — where a legal `initial_interval: 1ms` with `multiplier: 1`
would otherwise mean some 60,000 sends, and a jittered nanosecond interval a
CPU-bound loop against the destination. It never binds under the default
backoff, and it is a floor on the pace only: the stop conditions below are
unchanged.

**It stops** on the first of:

- the send succeeding;
- a non-recoverable error — a rejected message is never retried;
- the delivery's context ending, which happens on shutdown or a reconfiguration
  swap. The delivery is then left unsettled, for a redelivering source to
  deliver again, and spends no replay budget;
- the route wedging;
- the next wait ending past the budget.

All of those are re-checked when a wait ENDS, not only right after a send. The
loop spends almost all of its budget parked on a backoff timer, and its state
can change during that park: another delivery can latch the wedge, and the
bridge can cancel the delivery context. The budget is re-measured there too,
because a timer guarantees a MINIMUM delay and nothing more — scheduler pressure
or a GC pause can resume the loop past the budget the delay was measured
against, and the two validation rules below are sized on the last send STARTING
inside the budget. A delivery that wakes into any of these stops without a
further physical send.

Afterwards nothing changes. The same replay-cap gate runs and the message is
either handed back to its source or written to the dead-letter store exactly as
before. In-process retries do not spend the message's replay budget: the
bridge-owned replay ledger is still charged once per delivery, however many
sends that delivery took.

**The budget** is a new route policy field, `send_retry_budget`
(`routing.RoutePolicy.SendRetryBudget`). It applies to `direct_hold` only, the
mirror of `replay_budget` being read by the drainer only.

- Leaving it out takes the default of **60 seconds**
  (`routing.DefaultSendRetryBudget`). Sixty seconds rides out a destination
  policy that is still propagating — up to about a minute on SQS — and fits
  inside the limit that a held delivery must not outlive: the MQTT
  settlement-recovery recycle wait, 300 seconds with the shipped defaults,
  which the second validation rule below enforces per route.
  A shutdown is shorter than the budget and deliberately so: `Stop` waits about
  25 seconds for in-flight deliveries and then cancels, which **truncates** a
  held retry part-way through its budget — with the 60-second default, a
  graceful shutdown during a destination outage truncates one every time. A
  cancelled retry leaves the delivery unsettled, so a source that redelivers
  hands it to the next process; the budget is a ceiling on how long the bridge
  keeps trying, not a promise that it always gets the whole time.
  **A source that cannot redeliver loses the message there.** A best-effort
  (QoS 0) `direct_hold` source has nothing to redeliver, so an abandoned
  delivery is gone with no dead-letter record and no terminal accounting —
  where before this change its first failed send would have written it to the
  dead-letter store with its failure evidence. The window in which a
  cancellation can catch a delivery that way is no longer one `send_timeout`
  but the whole retry budget. A route whose source cannot redeliver should run
  a small budget, or `0s`.
- An explicit `0s` turns in-process retry off and restores the behaviour of
  every release before this one. It is the same tri-state as `jitter: 0`:
  programmatically it is `routing.SendRetryBudgetDisabled`, which is kept
  distinct from the zero value so a field that was simply left out still gets
  the default.
- Any other negative value is rejected when the configuration is loaded.

**Hooks and metrics.** `OnAttempt` and the `OnDelivery` callback fire once per
physical send, so a hook sees every one of them and their errors. Every
physical send of one delivery reports the **same** `DeliveryAttempt.Attempt`:
that number is the delivery-level attempt that `max_replay_attempts` caps, and
in-process retries do not advance it. `OnSettled` still fires exactly once.

Two counters, both tagged `route_id` and both emitted only while the budget is
enabled:

| Metric | Meaning |
|---|---|
| `SendRetries` | One per in-process retry after a recoverable send failure. |
| `SendRetryBudgetExhausted` | One per held send whose retries used up the budget. |

`RouteErrors` now counts a failed send once the in-process retry has given up,
not on the first failed send. A stalled destination therefore shows up in
`RouteErrors` up to the budget plus the last send's hold after the first send
started — that hold is at most one send wedge ceiling (defined below), so 95
seconds with the defaults — or never, when the destination recovers inside the
budget. `SendRetries` is the early signal to alert on instead.

**A waiting delivery keeps its slot.** It holds its route's `max_in_flight`
slot — and the runtime-wide in-flight slot, where a runtime-wide limit is
configured — for the whole wait, so intake slows down while a destination is
unwell. That is the intended effect: the source becomes the buffer instead of
the dead-letter store. A persistent MQTT session's broker keeps queueing for
the session, an SQS message stays invisible under auto-extend, and an AMQP link
runs out of credit. The cost is stated plainly: one route whose destination is
down can hold runtime-wide in-flight capacity for up to the budget, which slows
unrelated routes sharing it.

**Two validation rules keep the hold inside what the source will tolerate.**
Both run when the configuration is loaded, so a bad combination never reaches
production traffic. Both size the last physical send by the **send wedge
ceiling**, `send_timeout + min(send_timeout, 5s)`: a sender that ignores its
context keeps the delivery that long before the route gives up on it and
wedges. The validator and the dispatch path read the same function,
`route.SendWedgeCeiling`, so the two cannot drift apart. The ceiling applies
to `direct_hold` routes only, because only they send while the source is
still held. Every other delivery mode settles its source before it sends — a
`shared_outbox` route acks once the outbox record is persisted — so its
fixed-window sum keeps plain `send_timeout`, unchanged.

1. On a source with a **fixed** visibility window — an SQS queue without
   auto-extend — the budget is counted into the worst case the message may
   spend before it is settled:
   `processors × processor_timeout + send_retry_budget + send wedge ceiling +
   DLQ budget ≤ visibility timeout`. Over that, the source redelivers
   mid-pipeline and the message is processed twice. The rejection reads
   `worst-case pipeline time (… + SendRetryBudget … + SendTimeout … (+… wedge
   grace) + DLQ budget …) exceeds source VisibilityTimeout (…); source may
   redeliver mid-pipeline causing duplicate processing (lower
   send_retry_budget, set it to 0s to turn in-process send retry off, or
   auto-extend the source window)`. A source that auto-extends its window is
   skipped, as it always was.
2. On a source session that recycles its broker connection to recover stranded
   settlements — an MQTT persistent or exclusive session — that recycle first
   waits a bounded time for the deliveries the runtime already accepted to
   settle. A held retry still running when the wait runs out fails the recovery
   attempt and terminalizes the session. The last send starts just inside the
   budget and may then hold the delivery until its send wedge ceiling, so
   `send_retry_budget` + send wedge ceiling is the hold the wait has to cover.
   The rejection reads `send_retry_budget … + send_timeout … (+… wedge grace)
   exceeds the source's settlement-recovery wait …; a held retry would outlive
   the wait and fail the source's recycle (lower send_retry_budget or
   send_timeout, or raise the source session's settlement-recovery wait through
   its transport's own timeouts)`. For an MQTT session those timeouts are its
   connect and reconcile timeouts.

The session reports that wait through a new optional typed-config capability,
`ports.SettlementRecoveryTimingConfig`, which the MQTT transport implements
from the same numbers its own recycle uses. The validator and the adapter
therefore cannot disagree about how long a held delivery may take to settle.

**The second rule is necessary, not sufficient.** The recovery wait is the outer
deadline for the *whole* recovery attempt, not a budget reserved for settling:
the same 300 seconds also covers waiting for the session serialization gate, the
teardown drain, and the disconnect, reconnect and reconcile that follow it. And
one held delivery can occupy more of that wait than `send_retry_budget` + send
wedge ceiling: the message runs its processor chain first, where each processor
may take up to `processor_timeout`, and a message that ends up dead-lettered
spends a further 10.5 seconds on that write. With the shipped defaults and no
processors, the worst case one delivery can hold grew from about 45 seconds to
about 105 seconds of the same 240. The
rule catches the obvious overrun, and a long processor chain plus a dead-letter
write can still crowd the recycle on a route that passes it. The cure is a
smaller `send_retry_budget`, or larger session `connect_timeout` /
`reconcile_timeout` values, which is what the wait is computed from.

## Consequences

- A destination outage shorter than the budget no longer produces dead-letter
  entries or MQTT session recycles. Messages without a producer id — the common
  MQTT publish — benefit most: they were dead-lettered on the first failure and
  now get a full minute of retries first.
- A dead-letter redrive through the admin API inherits this. A redrive batch
  runs under a 30-second bound, and each entry's lookup and inject under the
  smaller of 10 seconds and what is left of the batch
  ([ADR 0019](0019-dlq-auto-redrive-by-system-event.md) amends this
  consequence). An entry whose destination stays down spends its bound
  retrying in process; when the bound passes the retry loop stops, the inject
  returns an error and the entry is **not** deleted (0015), so nothing is
  lost. The bound stops the retrying, not a send already in progress: against
  a sender that ignores its context the request can run up to one send wedge
  ceiling past the 30 seconds.
- Because each entry has its own bound, a slow first entry fails on its own
  and the later entries of the batch are still attempted. Entries the batch
  does not reach before its 30 seconds end still come back `redrive deadline
  exceeded before entry lookup` (or `inject failed: context deadline exceeded`
  on a store whose lookup ignores its context), and stay in the store:
  inject-then-delete is unchanged. Against a destination that is down each
  attempted entry can use its full 10 seconds, so a batch reaches about three
  entries; retry the failed ids once the destination is healthy.
- A synchronous `Inject` / `InjectToBinding` into a `direct_hold` route now
  returns only when the retry loop is done with it. Once the call has an
  in-flight slot it can block for up to
  `processors × processor_timeout + send_retry_budget + send wedge ceiling`,
  plus the 10.5-second dead-letter write when the message is dead-lettered
  (and a second one when the dead-letter store itself fails) — 105.5 seconds
  with the defaults and no processors. The caller's context is the real cap: when it ends, the loop
  stops and the delivery is abandoned without a dead-letter record, though a
  send already in progress is still waited for, up to its wedge ceiling. The
  [programmatic API guide](../programmatic-api.md) spells the bound out.
- **Duplicates can be amplified.** A send the destination accepted but whose
  response was lost is indistinguishable from a failure, and the retry
  publishes it again. That window existed before; what changes is its size —
  for an uncountable message it cost one extra downstream copy, and it now
  costs up to one per physical send the budget allows. The requirement is
  unchanged and stated in the transport matrix (`Downstream must dedupe`, see
  [MQTT behaviour](../transports/mqtt-behavior.md)); the budget decides how
  many copies that requirement has to absorb.
- **The route's `backoff` now decides how hard a failing destination is hit.**
  In-process retries are bounded by wall clock, not by a send count; before
  this change `max_replay_attempts` bounded the number of sends. The default
  ladder (1 s doubling to 30 s) makes a 60-second budget about six sends. An
  aggressive ladder — `initial_interval: 1ms` with `multiplier: 1`, both legal —
  is raised to the 100 ms floor, which still allows about 600 sends inside the
  same budget at a destination that is already unwell. Keep the default
  backoff, or lower the budget. No send-count cap is added: the budget, the
  route's own backoff and the 100 ms floor are the bound.
- Deliveries are held longer. Worst case one message occupies its route slot
  and a runtime-wide in-flight slot for the budget plus one send wedge ceiling,
  and a destination that is down slows the whole route rather than draining it
  to the dead-letter store.
- `Stop` waits up to its drain budget (25 seconds by default, `WithStopQuiesce`)
  for in-flight deliveries and then cancels, which ends a held retry and leaves
  the delivery unsettled. A source that redelivers replays it into the next
  process. A best-effort (QoS 0) source does not, so that message is lost with
  no dead-letter evidence, where on the old first-failure path it would have
  been dead-lettered; and because the 60-second default outlasts the 25-second
  drain, a shutdown during an outage hits that case rather than avoiding it.
- A configuration that was valid before can be rejected after an upgrade,
  purely because of the 60-second default: a `direct_hold` route on a fixed SQS
  visibility window whose worst case now overruns it, or an MQTT route whose
  budget plus send wedge ceiling overruns the session's recovery wait. Both
  rejections name the knobs, and `send_retry_budget: 0s` restores the previous
  behaviour on that route.
- The fixed-window sum counts the send wedge ceiling for **every `direct_hold`
  route**, not only one that retries in process, where it used to count
  `send_timeout`. A `direct_hold` route whose worst case sat within five seconds
  of its fixed window — the most the grace adds — is rejected after an upgrade
  even with `send_retry_budget: 0s`. That route could already hold its source
  past the window whenever a sender ignored its context. Lower `send_timeout`
  or a processor timeout, widen the window, or turn auto-extend on. Routes in
  any other delivery mode are validated exactly as before.
- `replay_budget` stays what it was: the drainer's wall-clock poison gate. The
  two budgets never apply to the same route.

## Alternatives considered

- **Tell operators to use `shared_outbox` instead.** The drainer already
  retries for 15 minutes, so a route that cannot afford to dead-letter on a
  short outage could switch modes. Rejected: `shared_outbox` requires a durable
  outbox store and acknowledges the source as soon as the record is persisted,
  which is a different delivery contract, not a retry setting. A route is on
  `direct_hold` because it wants the source held until the destination has the
  message; asking for a store and a weaker contract to survive a five-second
  blip is too large a change to ask for.
- **Leave it to the destination SDK's own retryer.** The AWS SDK inside the SQS
  sender already retries a few times. Rejected: it covers one transport. The
  MQTT and AMQP senders have no such layer, so the behaviour would stay
  transport-dependent — and each SDK's retryer sits inside a single
  `send_timeout`, so it cannot span the minute an outage lasts without making
  that timeout long enough to break the visibility-window checks.
- **Make `replay_budget` apply to `direct_hold` as well.** It is the field
  operators already reach for, and reusing it would add no new knob. Rejected:
  the two budgets mean different things. `replay_budget` is wall-clock measured
  from a record's first attempt across drainer claims, half of an AND-gate with
  `max_replay_attempts`, and its 15-minute default is safe only because the
  source was acknowledged long ago. Held against an unsettled source, 15
  minutes would outlive every visibility window and every MQTT recovery wait.
  One name for two incompatible defaults would have been the more confusing
  outcome, so `send_retry_budget` is its own field and the reference says
  plainly which mode reads which.
