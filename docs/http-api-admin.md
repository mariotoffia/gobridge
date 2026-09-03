# Admin API endpoints

Default listen address: `:8080`. All endpoints require authentication.
Responses use **method-prefixed route patterns** -- requests with the wrong
HTTP method receive HTTP 405 with a correct `Allow` header.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/bridge` | Instance info (`instance_id`, `running`, route count) |
| POST | `/api/v1/admin/bridge/start` | Start the bridge (`AdminOperationTimeout`, default 30s). Resume means a FRESH runtime -- a stopped one is single-use. Any previous runtime that is not running is stopped first, releasing its broker sessions, store handles and leases, so a runtime that tripped terminal (unhealthy but still flagged running) can never be orphaned beside the replacement |
| POST | `/api/v1/admin/bridge/stop` | Stop the bridge (`AdminOperationTimeout`, default 30s) |
| GET | `/api/v1/admin/routes` | List all routes with delivery/dispatch mode |
| POST | `/api/v1/admin/routes/{routeID}/inject` | Inject a test message (JSON payload, 1 MB limit) |
| GET | `/api/v1/admin/dlq` | DLQ summary (`configured`, `count`, `count_capped`) |
| GET | `/api/v1/admin/dlq/messages` | Paginated DLQ messages (filter by `route_id`, `category`, `since`, `before`) |
| GET | `/api/v1/admin/dlq/messages/{id}` | Single DLQ entry with full payload (audited as `dlq.read_payload`) |
| POST | `/api/v1/admin/dlq/redrive` | Redrive DLQ entries by ID (max 100); 207 on partial failure |
| POST | `/api/v1/admin/dlq/delete` | Delete DLQ entries by ID (max 1000) |
| POST | `/api/v1/admin/dlq/delete-by-filter` | Delete by filter (requires `confirm_delete_all` for an empty filter; a negative `limit` is rejected with 400, `limit` 0 means unbounded within the filter) |
| POST | `/api/v1/admin/dlq/purge` | Purge the **entire** DLQ (requires `confirm_purge_all: true`) |

Config-transaction endpoints (`/api/v1/admin/config...`) are registered only
when a config transaction manager is wired; see [Config transactions](#config-transactions).

All DLQ endpoints return HTTP 404 `{"error": "no DLQ store configured"}` when
no DLQ store is present. Redrive additionally returns 404
`{"error": "no DLQ admin store configured"}` when the DLQ is read-only.

### Route list response

Routes are returned wrapped in an object:

```json
{
  "routes": [
    {
      "id": "ingest",
      "delivery_mode": "direct_hold",
      "dispatch_mode": "single",
      "max_in_flight": 100
    }
  ]
}
```

### Inject request body

```json
{
  "subject": "devices/sensor-1/data",
  "payload": "eyJoZWxsbyI6IndvcmxkIn0=",
  "headers": { "x-source": "test" }
}
```

The `payload` field is base64-encoded. Reserved `x-bridge.*` header keys are
stripped automatically. Request body limited to 1 MB.

An optional `id` field supplies the envelope ID; omit it for a server-generated
one (an explicit empty string is a 400). The response is
`{"status":"injected","envelope_id":...}`, and it means the message reached its
destination. When the route instead PROCESSED the message and settled it without
delivering it -- dropped by its failure policy, discarded by a filter processor,
already expired, or written to the DLQ -- the endpoint answers **422** with the
reason, rather than reporting a delivery that never happened. 422 says the route
behaved as configured; a 500 still means the inject itself failed. A
**caller-supplied** `id` can collide
with a completed/poisoned outbox row on a `shared_outbox` route, where the
re-persist is swallowed as a duplicate and nothing is sent; the response then
carries a non-fatal **`warning`** field flagging the possible no-op
(server-generated IDs are unique, so they never trigger it).

### DLQ summary response

```json
{ "configured": true, "count": 42, "count_capped": false }
```

The summary scans up to 1000 entries. When that cap is hit, `count` is `1000`
and `count_capped` is `true`, signalling that the true backlog is at least that
large (there is no exact Count port, so alerting must treat a capped count as a
lower bound).

### DLQ messages -- pagination

The `/api/v1/admin/dlq/messages` endpoint supports pagination and filtering:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `limit` | int | 100 | Max messages to return (clamped to 1000) |
| `offset` | int | 0 | Number of messages to skip (max 100000) |
| `route_id` | string | -- | Filter by route ID |
| `category` | string | -- | Filter by error category |
| `since` | RFC 3339 | -- | Only messages failed at or after this time |
| `before` | RFC 3339 | -- | Only messages failed before this time |

```bash
curl -s -H "X-API-Key: change-me-to-a-real-secret-key" \
  "http://localhost:8080/api/v1/admin/dlq/messages?route_id=ingest&limit=10&offset=0" | jq .
```

```json
{
  "messages": [
    {
      "id": "dlq-001", "route_id": "ingest", "binding_id": "to-events",
      "source_id": "mqtt-in", "correlation_id": "corr-abc-123",
      "subject": "telemetry/temperature/sensor-42",
      "reason": "invalid payload", "category": "rejected",
      "error_code": "INVALID_PAYLOAD",
      "last_error": "json: cannot unmarshal string into Go value of type int",
      "failed_at": "2026-03-28T10:15:30Z", "attempts": 3
    }
  ],
  "limit": 10, "offset": 0, "has_more": false
}
```

The response no longer carries a `total` field (the old `total` reported
`min(matched, limit+offset)`, which under-reported once the backlog exceeded the
page window). Instead, **`has_more`** is a truthful "another page exists" flag,
computed by over-fetching one entry beyond the page. The list view omits the
payload; fetch a single entry via `GET /api/v1/admin/dlq/messages/{id}` to see
the full base64 payload -- that read is audited as `dlq.read_payload` because it
can disclose PII/secrets.

### DLQ redrive

```bash
curl -s -X POST -H "X-API-Key: change-me-to-a-real-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"ids": ["dlq-001", "dlq-002"]}' \
  "http://localhost:8080/api/v1/admin/dlq/redrive" | jq .
```

```json
{ "redriven": 2, "failed": 0 }
```

On partial failure the endpoint returns **HTTP 207 Multi-Status** with a
per-entry `errors` array (200 only when every requested entry redrove):

```json
{
  "redriven": 1,
  "failed": 1,
  "errors": [
    { "id": "dlq-002", "error": "entry already redriven or concurrently deleted" }
  ]
}
```

Maximum 100 IDs per request (duplicates are de-duplicated); request body limited
to 1 MB and strict-decoded (unknown JSON fields are rejected). Redrive is
**inject before delete**: the entry is re-injected first and removed only after
the inject is confirmed, so a failed *or refused* inject always leaves the entry
(and its failure evidence) intact -- never a delete after a no-op. The cost is a
bounded at-least-once window: a crash between a confirmed inject and the delete
re-drives on the next attempt. Replay is **binding-scoped** -- the entry's
originating `binding_id` is carried **out-of-band** through the runtime's
redrive-safe injection capability so a fan-out route re-delivers only to the
binding that failed, not to its healthy siblings. It is **not** a header:
`x-bridge.route-override` and the other reserved `x-bridge.*` keys are stripped
at ingress before any consumption site reads them, so a header cannot steer the
replay.

An inject is "confirmed" only when the route actually delivered the message. A
replay the route **dropped** (`on_permanent_failure: drop`), filtered, expired,
or wrote back to the DLQ is reported per entry in the `errors` array, counted on
`DLQRedriveFailures`, and its entry is **kept** -- deleting it would destroy the
message and the only record that it failed. On a route that retains failures,
such a redrive also writes a NEW entry for the new failure, so one message shows
two entries; the newer one's envelope carries `x-bridge.causation-id` set to the
original envelope ID.

Redrive uses **redrive-safe injection** (`InjectRedrive`) where the runtime
supports it: the message is re-issued under a fresh envelope ID with the
original stamped as provenance (`x-bridge.causation-id`). The re-issue also
drops the source's **adapter-generated identity marker**
(`x-bridge.generated-id`): that marker means the SOURCE supplied no stable
identity, which makes a message uncountable and sinks it terminally on its first
transient failure. A redrive is operator-issued under the fresh bridge-minted
ID, so it is countable and gets the route's normal retry budget. The re-issue
also **drops the stale transport dedup key** (`x-bridge.dedup-id`): a redrive is a
deliberate operator re-issue, so the original dedup id -- whose whole purpose is
to *suppress* re-delivery (e.g. it maps to an SQS FIFO `MessageDeduplicationId`)
-- must not ride along, or an idempotent/FIFO sender would swallow the "fresh"
replay and the redrive would delete the entry after a no-op. Non-suppressing
keys (ordering-key, correlation-id) and the causation provenance are preserved.
Reusing the original ID is a silent-loss path on `shared_outbox` routes -- the
outbox retains completed/poisoned rows as dedup evidence keyed on
`(envelope_id, binding_id)`, so re-persisting the same ID is swallowed as a
duplicate, the dispatch ACKs, and the redrive reports the entry redriven while
nothing is sent.

When the runtime **lacks** `InjectRedrive`, redrive behaviour depends on the
entry's scope:

- **`shared_outbox` / binding-scoped entries are refused.** An entry that
  records a non-empty `binding_id` would, on any legacy path, replay under the
  original envelope ID and be swallowed by the outbox dedup row -- deleting the
  DLQ entry after a no-op (silent loss of both the message and its evidence). So
  the redrive is refused **before any inject** and the entry is **not** deleted;
  its message and failure evidence are preserved. The per-entry error reads
  `refused: runtime lacks redrive-safe injection ...` and the batch returns
  `207`. Upgrade the runtime to one that implements `InjectRedrive` (the
  built-in runtime does).
- **Direct entries** (empty `binding_id`) still replay via a plain `Inject`
  **only when they are collision-free** -- an empty envelope ID **and** no
  `x-bridge.dedup-id` header -- so the runtime assigns a fresh ID and the
  transport re-derives dedup from it. A direct entry that carries a non-empty
  envelope ID **or** a dedup key is **refused** for the same reason as a
  shared_outbox entry (an idempotent/FIFO sender could silently deduplicate the
  replay and the entry would be deleted after a no-op); it returns `207` with a
  per-entry `refused: runtime lacks redrive-safe injection ...` error and the
  entry is preserved. A collision-free direct replay still carries a non-fatal
  **`warning`** field so a bare success does not hide a possible no-op; verify
  delivery in that case:

```json
{
  "redriven": 2,
  "failed": 0,
  "warning": "runtime lacks redrive-safe injection: replays reuse the original envelope id and may be silently deduplicated by the outbox on shared_outbox routes; verify delivery"
}
```

Before the inject→delete loop, redrive emits a `dlq.redrive.begin` audit record
(outcome `pending`) carrying the requested IDs, so a crash between a confirmed
inject and its delete still leaves a trace of which entries were being redriven
(and which may re-drive on the next attempt) -- the per-batch `dlq.redrive`
outcome record is written only if the handler returns.

If the recorded `binding_id` no longer exists on a still-present but
reconfigured route, the redrive fails that entry with `route or binding not
found` (a permanent `ErrNotFound`) and the DLQ entry is preserved -- the failing
inject never reaches the delete -- rather than being fanned out to the route's
current bindings. Re-file the entry against a binding that exists.

### DLQ purge

```bash
curl -s -X POST -H "X-API-Key: change-me-to-a-real-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"confirm_purge_all": true}' \
  "http://localhost:8080/api/v1/admin/dlq/purge" | jq .
```

```json
{ "purged": 5 }
```

Purge deletes the **entire** DLQ (all failure evidence), so it requires an
explicit `{"confirm_purge_all": true}` body; omitting or setting it false
returns HTTP 400. Request body limited to 1 MB.

### Config transactions

When a config transaction manager is wired, the admin server exposes
`/api/v1/admin/config` (GET the redacted effective config) plus a transaction
lifecycle under `/api/v1/admin/config/transactions`:

| Method | Path | Notes |
|--------|------|-------|
| POST | `.../transactions` | Open a transaction against the current version |
| GET | `.../transactions/{txnID}` | Inspect the pending (redacted) preview |
| PATCH | `.../transactions/{txnID}` | Apply a partial `BridgeConfig` overlay |
| POST | `.../transactions/{txnID}/commit` | Validate, CAS on version, persist, apply |
| DELETE | `.../transactions/{txnID}` | Roll back / discard |

The overlay is strict-decoded as a `BridgeConfig`; because typed plugin options
are not JSON-serialisable fields (`Config` carries `json:"-"`), a PATCH body
containing an `options` key is an **unknown field → HTTP 400**. A PATCH may
therefore only touch scalar/structural fields, never a plugin's option block.

**PATCH is merge-only.** Every PATCH runs `store.Merge` against the current
on-disk config (`config.DefaultMerge`), so omitted, empty-string, empty-list, and
the redaction marker (`"[REDACTED]"`, the value echoed back by a redacted read) all
**preserve the current value** — a field cannot be cleared or removed via PATCH.
The transaction API has **no replacement mode**: a PATCH carrying the full desired
config still merges, so it cannot clear a field either. To remove a field, rewrite
the underlying config document at its source — edit the config file (the file
watcher reloads the whole document) or overwrite the config-store item — so the
whole document is reloaded instead of an overlay merged.
Fields that cannot be cleared via PATCH include: `http.cors_origins`,
`http.tls_cert_file` / `http.tls_key_file`, `http.admin_api_key` /
`http.monitor_api_key` (empty **or** `"[REDACTED]"` keeps the stored secret; note
that any *other* non-empty string overwrites the secret verbatim), a
receiver/sender/binding `session_id`, and a receiver's `topics` list.

Commit refuses to erase plugin options: if the merged config would drop the
typed `Config` of any entry that previously had one -- most commonly by changing
a `transport`/`type` discriminator via PATCH (which cannot carry replacement
options) -- the commit fails with **HTTP 422** and `errConfigOptionsLoss`
("config commit would erase plugin options"). Other outcomes: `404`
(transaction not found/expired), `409` (version conflict / concurrent write),
`422` (config validation failed, with `validation_errors`).

Commit is **write-then-apply**: the new config is durably written to the store,
then applied in-band to the running runtime on a context detached from the request
(so a client disconnect cannot tear the durable write from the apply). The apply is
bounded by a 60s cap; exceeding it counts as an apply failure (`rolled_back`), not
an in-flight result. The response `status` reports the outcome:

| `status` | HTTP | Meaning |
|----------|------|---------|
| `committed` | 200 | Applied to the running runtime (or no `ConfigApplier` wired). `{"status": "committed", "version": N}`. |
| `committed_applying` | 202 | The durable write succeeded and the runtime accepted the config, but the applier reported the swap is still in flight (`ErrApplyInFlight`), or the bridge is paused/shutting down and recorded it for a later resume. This is a signal from the applier, not the 60s cap being hit. The write is **retained** on disk and the runtime is converging -- no action required. |
| `rolled_back` | 500 | The apply failed (including exceeding the 60s apply cap) and the previous on-disk config was restored, so a process restart recovers the last good config instead of crash-looping. `version` is the restored previous version. |
| `committed_not_applied` | 500 | The apply failed **and** the previous config could not be restored (first write, or the restore write itself failed). Disk holds the rejected config; disk and the running runtime have diverged and an operator must reconcile. |

### Audit logging

All admin endpoints emit audit events with timestamp, action, actor, resource
type, resource ID, outcome (`success` / `failure` / `partial_failure`), and an
optional detail map. On a successful admin authentication the **actor** is the
matched admin key name (see [Named admin keys](http-api.md#named-admin-keys)); the network
address — the leftmost `X-Forwarded-For` hop when present, else `RemoteAddr` — is
recorded separately as `Detail["client_addr"]`. `client_addr` is display-only and
spoofable unless a trusted proxy normalises `X-Forwarded-For`.

An `auth.failure` (or `auth.throttled`) event is emitted before any successful
match, so it has no key identity: its actor stays the network address.

**Migration note.** Deployments that used the single `AdminAPIKey` previously
recorded the client network address as the audit `Actor`. After upgrading, those
requests record `Actor: "admin"` (the reserved fold-in name) and the address moves
to `Detail["client_addr"]`. Audit consumers that parse `Actor` as an IP must read
`Detail["client_addr"]` instead.

Notable actions include `bridge.status`, `bridge.start`, `bridge.stop`,
`route.inject`, `dlq.redrive`, `dlq.purge`, `config.txn.patch` /
`config.txn.commit` / `config.txn.rollback`, and -- new in this release:

| Action | Emitted when |
|--------|--------------|
| `auth.failure` | An API key check fails (also increments the per-client throttle) |
| `auth.throttled` | A request is rejected with 429 because the client is over its failure limit |
| `dlq.read_payload` | A single DLQ entry (with full payload) is read via `GET .../dlq/messages/{id}` |
| `dlq.redrive.begin` | Emitted (outcome `pending`) before the inject→delete loop of a redrive batch, recording the requested IDs |

`route.inject` records outcome `not_delivered` (alongside `success` and
`failure`) when the route processed the message and settled it without
delivering it.
