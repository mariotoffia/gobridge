---
applyTo: "httpapi/**,spec/httpapi/**"
---

# Admin and monitor HTTP API

Sources: ADR-0004, ADR-0012, ADR-0015, ADR-0019,
`docs/internals/architecture-operational-surfaces.md` §10 and
`docs/internals/architecture-contracts-and-clustering.md`. ADR-0006
(delete-first redrive) is superseded by ADR-0015; do not cite it as current.

- DLQ redrive is inject-then-delete (at-least-once): `DLQReader.Get`, then
  `Runtime.InjectRedrive`, then `DLQAdmin.Delete`. A failed inject keeps the
  entry. A failed delete is reported as "message re-injected but DLQ entry not
  removed". `deleted == 0` is not an error.
- Redrive runs on `context.WithoutCancel` with the bounded `redriveTimeout`,
  so a client disconnect cannot stop it halfway.
- Each entry's `Get` and inject run under `redriveEntryTimeout` (10 s), derived
  from the batch context, so one slow entry cannot starve the rest. The delete
  after a confirmed inject runs under the batch context only. A lookup that
  fails once the entry context has ended reports `redrive deadline exceeded
  before entry lookup`, never "entry not found" (ADR-0019).
- `dlqEntryView` carries `redrive_mode` and `extra_info`; `extra_info` is
  always a JSON object (`{}` when empty), never `null`.
- The response is 200 when every entry succeeds and 207 when any fails;
  failures are counted on `DLQRedriveFailures`. A missing binding reports
  "route or binding not found".
- A runtime without `InjectRedrive` refuses binding-scoped entries and entries
  that carry an ID or a dedup key.
- `/live` checks only `Terminal()` / `Healthy()` and knows nothing about
  bootstrap internals.
- Bare `/ready` requires `full`; a standby is capped at `subscribed` and
  answers 503. `role` is `standalone`, `active` or `standby`.
- An admin config commit on a clustered deployment is not a rollout shortcut:
  a live apply is rejected unless the rollout is coordinated (ADR-0012,
  ADR-0013).
- `POST /admin/bridge/start` always builds a fresh runtime (ADR-0004).
- A change to a request or response shape updates `spec/httpapi/` in the same
  PR.
