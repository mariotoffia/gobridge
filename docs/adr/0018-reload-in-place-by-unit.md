# 0018 — Reload in place by reload unit

Status: accepted
Date: 2026-09-23
Deciders: GoBridge core
Amends: 0004 (a running runtime may retire and graft reload units; it is still
started once, stopped once and never restarted)
Relates to: 0016 (a reload unit is identified by its content normal form)

## Context

Every configuration change that was not a no-op replaced the whole runtime.
The Supervisor (`cmd/gobridge`) and the AWS runtime
(`deployment/aws/lib/bootstrap`) built a new runtime from the new document and
stopped the old one, in one of two orders:

- **overlap** — build the new runtime while the old one still runs, then hand
  over: the Supervisor stops the old runtime and starts the new one, and the
  AWS runtime starts the new runtime before it stops the old one;
- **prepare/commit** — validate the new document and prepare its build while
  the old runtime still runs, then stop the old runtime, and only then create
  the new sessions, receivers and senders and start the new runtime, because a
  session holds an exclusive broker identity (an MQTT client ID, an exclusive
  AMQP consumer, a pinned Service Bus session) that two runtimes cannot hold at
  once.

Either way every session disconnected and connected again, and every route
stopped and started. One configuration often serves several owners — tenants,
teams, device classes — each with their own sessions, receivers, senders,
bindings and routes. Adding one owner's route disconnected every other owner's
broker session. On MQTT, QoS 1/2 on a persistent session was replayed after the
reconnect, with possible duplicates, while QoS 0 and anything an ephemeral
session would have received during the window was lost. ADR 0016 stopped
reloads for changes that change nothing; a real change still cost every owner
an outage.

ADR 0004 made the runtime single-use: it starts once, stops once and is never
restarted, because resetting everything a runtime owns is where leaked
goroutines, connections and stale leases come from. Its consequence read "a
config change builds a fresh instance or it fails". A finer-grained reload has
to keep the reason behind that rule.

## Decision

**A change confined to sessions, receivers, senders, bindings and routes
reloads the running runtime in place, one reload unit at a time.** Every other
change keeps the full replacement.

**The reload unit.** A reload unit is one connected group of the
configuration's sessions, receivers, senders, bindings and routes, joined by
every reference the builder follows: a receiver's, sender's or binding's
`session_id`, a binding's `sender_id`, and a route's `receiver_id`, `bindings`
and `session` block. Processors are shared by name and count as part of a
route's content, not as members. A session id that something names belongs to
the unit whether the document declares that session or not.

A unit is identified by the content normal form (ADR 0016) of a document that
holds the bridge-wide sections plus exactly that unit's members. A unit with
the same identity in the running and the next document is unchanged and keeps
running untouched. Every other unit of the running document is **retired**, and
every other unit of the next document is **added**. Old and new units are never
paired: a change inside a unit retires the old unit and adds the new one, and a
merge or a split of units is handled the same way.

The unit is the smallest thing a reload replaces. Two routes that share a
session are one unit, so changing either route reconnects that session and
restarts the other route as well. Replacing a single route on a live session
would need a receiver rebuilt against a running session; it is left out until a
shared session per owner becomes the common shape.

**When it applies.** `bridge.PlanInPlaceReload` decides. It refuses, and the
reload takes the full replacement exactly as before, when:

- the bridge-wide part differs: `bridge` settings, `config_watch`, `stores` or
  `http`. The running runtime was built for those;
- the outbox stale-claim duration a build derives differs. The outbox store
  takes it when it is opened, and an in-place reload keeps the running store
  open. Unless `stores.outbox` sets `stale_claim_duration`, it is derived from
  the largest `step_down_grace` of any route session;
- a retired or added unit attaches to a transport that serves an HTTP endpoint
  (`ports.CapHTTPEndpoint`, which the `http` transport declares), or to a
  transport with no registered factory. The HTTP transport mounts its paths on a
  standard-library `ServeMux`, which can neither unmount a path nor mount the
  same path twice.

The caller also needs a running runtime. The Supervisor reloads in place only
under `SwapAuto`, the default. `WithSwapMode(SwapInPlace)` acts as `SwapAuto`;
an explicit `SwapOverlap` or `SwapPrepareCommit` keeps the full replacement it
asks for. The AWS runtime always tries in place first. Each root runs the
checks its own full swap runs before an in-place reload changes anything: the
Supervisor its no-op detection, clustered-reload guard, and store-identity,
lease `session_id` and durable-backlog preflights; the AWS runtime its no-op
content fingerprint, deployment-profile admission and cluster reload seam. So
neither root's in-place reload lets through a change its own full swap would
refuse.

**How it runs.** `(*bridge.InPlaceReload).Apply` works in this order:

1. It validates the whole next document with the builder's full preflight, and
   plans a part for every added unit: the unit's own preflight, and the checks
   a full build runs over its stores, applied to the running runtime's stores.
   A part is a runtime built from one added unit over those stores and never
   started; it is built in step 2 or after step 3. Nothing is opened in this
   step, and a failure changes nothing.
2. When `bridge.RequiresSerializedSwap`, asked of the retired units against the
   added ones, is true — an added unit claims an exclusive broker identity, or a
   retired unit holds one on a transport an added unit still attaches to — the
   reload is serialized: the parts are built only after step 3, so no identity
   is ever held twice. Otherwise the parts are built now, while the retired
   units still serve, and a failed build changes nothing.
3. It retires each retired unit with `runtime.Retire`: the unit's routes and
   sessions leave the runtime, its in-flight deliveries settle within the stop
   drain budget, its route runners, outbox drainers and session managers stop,
   its sessions close and release their leases, and credential refreshers stop
   watching its transports. Each retire is bounded by `bridge.drain_timeout`.
4. It grafts each part with `runtime.Graft`, which moves the part's routes and
   sessions into the running runtime and starts them under the runtime's own
   settings.

Inside one runtime a replacement is always stop-then-start. A route id names
one route runner and a session id one session manager, so the old and the new
generation of a unit never run side by side. The AWS runtime's zero-gap overlap
therefore remains only for a full replacement. Unchanged units see no gap.

**How it fails.** Each failure ends like the matching failure of a full swap:

| Outcome | What the runtime runs afterwards | What the caller does |
|---|---|---|
| applied | the next configuration | publishes it as the applied configuration |
| unchanged | still the running configuration: nothing was retired, or the retired units were rebuilt from the running configuration and grafted back; a runtime that a shutdown, terminal failure or configuration fence stopped meanwhile stays with that path, as it would without a reload | reports the error and keeps the runtime |
| torn | neither: a failure after retiring could not restore the retired units, or the runtime stopped running before the rest were retired | stops the runtime and builds the running configuration afresh; if the stop fails, it wedges |
| wedged | unknown: a retired unit, or a part a serialized reload built or restored, did not stop cleanly, so its sessions may still hold their broker identities | stops the runtime and wedges, so the orchestrator restarts the process (ADR 0004) |

The Supervisor reports an in-place reload as `SwapEvent.SwapMode ==
SwapInPlace` and logs `retired_routes`, `added_routes`, `retired_sessions` and
`added_sessions` on success. The AWS runtime logs the same fields together with
the outcome.

**This amends ADR 0004.** The runtime is still started once and stopped once,
and it is never restarted in place: `Start` on a stopped runtime still fails.
What changes is that a running runtime may now retire reload units and graft
units built as a fresh part. A retired unit's components are stopped and
dropped, never reset for reuse; a grafted part is built new and is consumed by
the graft. "A config change builds a fresh instance or it fails" now reads: a
config change builds fresh components — a whole runtime, or parts grafted onto
the running one — or it fails.

## Consequences

- Adding, removing or changing one owner's pipes reconnects only that owner's
  units. Every other session stays connected, and every other route keeps
  delivering through the reload.
- Changing any member of a unit restarts the whole unit; routes that share a
  session are replaced together.
- A bridge-wide change, a change to a unit with an HTTP endpoint, or a change
  to the derived stale-claim duration still replaces the whole runtime, with
  the same gaps as before. Setting `stale_claim_duration` explicitly keeps a
  `step_down_grace` change in place.
- A replaced unit on a stateless transport (SQS, Service Bus without a pinned
  session) now has a short gap — its drain plus its start — that the AWS
  runtime's overlap swap used to avoid by starting the new runtime first.
  Messages wait at the source meanwhile. Only that unit sees the gap.
- The AWS runtime's MQTT memory profile divides one reservation equally among
  every MQTT session that can receive — every session a receiver uses, and
  every persistent or exclusive session in use, pinned or not. A session that
  leaves `ingress_memory_budget_bytes` unset takes its share as its budget,
  written into its configuration. So adding or removing one such MQTT session
  changes the share of every other unpinned MQTT session, and each of their
  units is replaced. Pinning `ingress_memory_budget_bytes` per session, at or
  below its share, keeps the other units connected; see
  [keeping MQTT tenants connected](../aws-deployment/config-reload.md#keeping-mqtt-tenants-connected).
- A reload that fails after retiring units ends unchanged when the retired
  units are restored from the running configuration; otherwise it ends torn (a
  full rebuild of the running configuration) or wedged (a process restart).
  Every check that can fail before the first retire runs before it.

## Rejected alternatives

- **Replace a single route on a live session.** It would make the unit smaller
  for routes that share a session, but it needs a receiver rebuilt against a
  running session and a session manager whose subscription plan changes while it
  runs. Rejected for now: routes on separate sessions — the usual multi-owner
  shape — already reload independently, and the ceiling is documented.
- **Pair old and new units by id and patch the difference.** Rejected: members
  can move between units, and units can merge or split. Comparing whole units by
  content needs no pairing rules and cannot miss a change.
- **Overlap inside the runtime: start the new unit before stopping the old
  one.** Rejected: the runtime indexes route runners and session managers by id,
  and both generations of a changed unit use the same ids. For an exclusive
  identity it would also claim that identity twice.
- **Reset the running runtime for bridge-wide changes.** Rejected by ADR 0004:
  resetting owned resources is the leak it guards against. Bridge-wide changes
  keep the full replacement.
- **Unmount and remount HTTP endpoints.** Rejected: the standard-library
  `ServeMux` offers neither. Units with an HTTP endpoint keep the full
  replacement.
- **A new AWS setting that sizes MQTT memory shares independently of the
  session count.** Rejected: the existing per-session
  `ingress_memory_budget_bytes` already decouples a session.
