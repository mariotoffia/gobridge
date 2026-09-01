# HTTP API Reference

> Part of the [Credential Management and HTTP API](credentials-and-http-api.md)
> reference. See also [HTTP API Examples](http-api-examples.md).

## HTTP API Configuration

The HTTP servers are enabled by including an `http` block in the bridge
configuration. Two servers run on separate ports: an **admin server** for
control operations and a **monitor server** for health and observability.

### Configuration fields

```yaml
http:
  admin_addr: ":8080"
  monitor_addr: ":8081"
  admin_api_key: "my-secret-api-key-min-16-chars"
  monitor_api_key: ""
  cors_origins: ""
  # Opt-in in-process TLS: set BOTH to serve HTTPS on the admin and
  # monitor listeners, or neither to stay plaintext behind an external
  # terminator. Setting only one is a startup error.
  tls_cert_file: ""
  tls_key_file: ""
```

| Field | YAML key | Type | Default | Description |
|-------|----------|------|---------|-------------|
| AdminAddr | `admin_addr` | string | `":8080"` | Admin server listen address |
| MonitorAddr | `monitor_addr` | string | `":8081"` | Monitor server listen address |
| AdminAPIKey | `admin_api_key` | string | *required* | API key for admin endpoints (min 16 chars) |
| MonitorAPIKey | `monitor_api_key` | string | *optional* | Separate key for monitor; falls back to `admin_api_key` |
| CORSOrigins | `cors_origins` | string | `""` | Comma-separated allowed origins; wildcard `*` is rejected |
| TLSCertFile | `tls_cert_file` | string | `""` | PEM server certificate (+chain); enables HTTPS when set with the key |
| TLSKeyFile | `tls_key_file` | string | `""` | PEM private key; **both** cert and key must be set, or neither |

> **Restart required.** The admin and monitor servers are bound once, when the
> process starts, from the configuration it booted with. Changing
> `admin_addr`, `monitor_addr`, `cors_origins`, `tls_cert_file` or
> `tls_key_file` through a reload (file or admin config API) is accepted and
> stored durably, but the running listeners keep their original settings until
> the process is restarted. Adding an `http` block to a process that started
> without one likewise creates no servers. The API keys are the exception --
> a deployment that resolves them through a secret provider picks up a rotation
> on the next request. Where the composition root can see the divergence it
> reports it in the `restart_required` field of the `/deephealth`
> `config_watch` projection.

TLS is opt-in and applies to **both** the admin and monitor listeners. When
both `tls_cert_file` and `tls_key_file` are present the servers serve HTTPS and
the reported `AdminURL`/`MonitorURL` use the `https` scheme; when both are empty
the servers stay plaintext (the historical default) on the assumption that an
external terminator (LB/ingress/mesh) provides TLS. Supplying exactly one of the
pair is rejected at startup.

Renewed certificates hot-reload in place. The server checks the cert and key
file modification times on each TLS handshake and reloads the pair when either
changes, so a cert-manager renewal is served without a restart. A reload that
reads a half-written pair keeps the last-good certificate and retries on the
next change.

### Authentication failure throttling

The admin and monitor auth middlewares each throttle repeated **failed**
authentication **per client**, in **separate** scopes: exhausting the admin
throttle never locks out the monitor plane, and vice versa. The credential is
checked **first**, so a valid key always authenticates even when its peer's
window is throttled by someone else's failures — only a failed credential
consults and feeds the throttle. Because the key is checked first, the throttle
shapes the failure *response* (429 vs 401) but does not cap the rate at which
keys are *tested*: online brute-force resistance rests on key entropy, so use
high-entropy admin/monitor keys (the 16-char minimum enforces length, not
randomness). After `AuthFailureLimit` failed attempts within
`AuthFailureWindow`, further **failing** attempts from that client are rejected
with **HTTP 429** and a `Retry-After` header (the full window — `60` at the `1m`
default) until the window rolls over; a successful auth resets the client's
counter. These knobs live on the programmatic `httpapi.Config` (they are not
part of the YAML `http:` block):

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `AuthFailureLimit` | int | `5` | Failed attempts per client per window before 429 |
| `AuthFailureWindow` | duration | `1m` | Fixed window over which failures are counted |

The throttle key is the transport peer: the host portion of `RemoteAddr`, port
stripped so every connection from one peer shares a window. It **ignores
`X-Forwarded-For`** and never uses an admin key name, so a client cannot partition
or evade the throttle by rotating a spoofed XFF hop or enumerating key names.

Audit attribution is separate: on a successful admin match the actor is the
matched key name (see [Named admin keys](#named-admin-keys)), else the network
address (leftmost `X-Forwarded-For` hop else `RemoteAddr`). **`X-Forwarded-For`
is client-spoofable** and authoritative only where a trusted proxy normalises it.

### Authentication

All authenticated endpoints accept credentials via:

- **Header**: `X-API-Key: <key>`
- **Bearer token**: `Authorization: Bearer <key>`

Key comparison uses **SHA-256 hashing followed by constant-time comparison**
(`crypto/subtle.ConstantTimeCompare`) to prevent both timing attacks and
length-based information leaks. With several [named admin keys](#named-admin-keys)
configured, each is compared the same way, so match timing leaks at most the
number of keys, never key material. Failed authentication returns HTTP 401 with a
`WWW-Authenticate: Bearer realm="gobridge-admin"` (or `gobridge-monitor`)
header per RFC 9110.

### Named admin keys

The admin API supports multiple **named** admin keys, so each operator carries a
distinct credential. Possession of a key is the identity: on a successful match
the audit `Actor` becomes the key's name (for example `alice`) — a stable principal
that survives a shared proxy and can be revoked one operator at a time.

Named keys are programmatic `httpapi.Config` fields — like the throttle knobs
above, they are not part of the YAML `http:` block:

| Field | Type | Description |
|-------|------|-------------|
| `AdminAPIKeys` | `map[string]shared.Secret` | Key name → key. Each name becomes the audit `Actor` when that key authenticates. |
| `AdminAPIKeysProvider` | `func() map[string]string` | Rotation hook. When set, it replaces the static `AdminAPIKeys` per request; each value is wrapped in a redacting `shared.Secret` before comparison. Mirrors `AdminAPIKeyProvider`. |

The legacy single `AdminAPIKey` / `AdminAPIKeyProvider` remain supported and
**fold in under the name `admin`**. When both are set, the map wins on a name
collision — an explicit `admin` entry in `AdminAPIKeys` overrides the legacy
field. At least one admin key must exist after folding, or `Start` fails with
`admin API key is required`.

Validation runs at `Start` over the folded set:

- Every key, including the folded `admin`, must be at least 16 characters.
- Every name must be non-empty, match `[a-z0-9._-]+`, and be at most 64 characters.
  They appear in audit logs and metric tags, so non-conforming names are rejected.

Matching is constant-time. The presented credential (from `X-API-Key` or the
bearer token) is hashed once into a 32-byte digest, then compared against each
stored key's SHA-256 with `crypto/subtle.ConstantTimeCompare`. The iteration
leaks at most the number of configured keys — never key material or key length.

On a successful admin match the audit `Actor` is the matched key name, and the
network address (leftmost `X-Forwarded-For` hop else `RemoteAddr`) moves to
`Detail["client_addr"]`. `client_addr` is display-only and spoofable unless a
trusted edge proxy normalises `X-Forwarded-For`. A failed authentication has no
key identity, so its `auth.failure` actor stays the network address (see
[Audit logging](#audit-logging)).

**Monitor endpoints stay single-key.** The monitor API is read-only telemetry
(monitor key, or the admin key as a superset); it is not per-operator attributed.
When an admin named key is used there, its name still flows to the audit context.

#### Named keys from an SSM parameter

The `aws-filebased-config` deployment profile can populate named keys from the
single admin SSM parameter. The resolved value is read by shape:

- A value whose first non-space byte is `{` is parsed as a JSON object of
  name → key and feeds `AdminAPIKeys` / `AdminAPIKeysProvider`:

  ```json
  { "alice": "alice-key-min-16-chars", "bob": "bob-key-min-16-chars" }
  ```

- Any other value is the legacy single key under the name `admin`.
- Malformed JSON whose first non-space byte is `{` is a hard startup error; it is
  never treated as a literal key (which would silently create a key equal to the
  JSON text).

The same detection runs on rotation; see the
[AWS deployment configuration](aws-deployment/configuration.md#admin-key-parameter-value).

#### Upgrade path

Named shared keys are the deliberate minimal step toward operator identity:
attribution and revocation without an identity provider. OIDC / mTLS / JWT operator
auth is a **non-goal** here — the next step once an IdP dependency is acceptable.
There are no per-key scopes (every key is full admin); the map value can grow a struct.

### Middleware chain

Every request passes through (in order): request logging, security headers
(`X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`), CORS (when configured), panic recovery, and
correlation ID injection (`X-Correlation-ID`, `X-Trace-ID`, `X-Span-ID`).

## Admin API Endpoints

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
matched admin key name (see [Named admin keys](#named-admin-keys)); the network
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

## Monitor API Endpoints

Default listen address: `:8081`.

### Unauthenticated (health probes)

All health probes set `Cache-Control: no-cache, max-age=0` to prevent stale
caching by load balancers and CDNs.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/health` | Coarse health check -- returns **only** `{"status": ...}` |
| GET | `/api/v1/monitor/live` | Liveness probe (`{"status":"alive"}`, or `{"status":"terminal"}` + 503) |
| GET | `/api/v1/monitor/ready` | Readiness probe; optional `?level=` gate (`{"status":"ready","role":...}` or 503) |

The `health` endpoint is **unauthenticated** (for load balancers/orchestrators)
and therefore exposes only a coarse `status` string plus the HTTP status code --
never `instance_id`, route count, or component-failure detail, which are
reconnaissance and live behind auth on `/deephealth`. It returns HTTP 200 with
`{"status":"ok"}` when healthy, and HTTP 503 with `{"status": ...}` otherwise,
where `status` is one of `ok`, `unhealthy`, `not_running`, or `unavailable`
(runtime not wired).

The `ready` endpoint accepts an optional **`?level=`** query parameter that sets
how strict the gate is, least to most strict: `live`, `running`, `connected`,
`subscribed`, `full`. Without it, the legacy contract applies -- HTTP 200
`{"status":"ready","role":...}` once the runtime is started and healthy, HTTP 503
`{"error":"not ready"}` otherwise. With it, the response is
`{"status":..., "role":..., "level":..., "requested":...}` and returns HTTP 503
when the achieved `level` is below the `requested` one; an unknown level returns
HTTP 400. `role` is the failover role from lease ownership: `active`, `standby`,
or `standalone`. See
[Health checks and graceful shutdown](deployment-guide.md#health-checks-and-graceful-shutdown)
for probe-to-level mapping.

An instance that carries no routes and no sessions never rises above `running`,
so the legacy no-`level` probe answers 503 for it. This is deliberate: every
"all routes are ready" test is trivially satisfied when there are no routes, and
without the cap a bridge that started without a configuration would present
itself as a fully working member of the pool.

`live` reports the state of the **process**, not only of the runtime object it
can see. A composition root that wires its supervisor's terminal state into the
server answers 503 as soon as that supervisor is wedged -- a reconfiguration
swap and its recovery both failed, so there is no active runtime and nothing is
routed. Without that signal a wedged process is indistinguishable from a normal
swap window and would keep answering 200 forever.

### Authenticated

Sensitive endpoints require the monitor API key or fall back to the admin key
(admin access is a superset of monitor access).

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/topology` | Bridge topology (instance ID, running state, compact route list) |
| GET | `/api/v1/monitor/routes` | Route details with full policy (max_in_flight, ack_after, on_expired, on_perm_failure, on_filtered) |
| GET | `/api/v1/monitor/deephealth` | Deep health check (sessions, routes, service levels) |

### Topology response

```json
{
  "instance_id": "gobridge-1",
  "running": true,
  "routes": [
    { "id": "ingest", "delivery_mode": "direct_hold", "dispatch_mode": "single" }
  ]
}
```

### Monitor routes response

```json
{
  "routes": [
    {
      "id": "ingest",
      "delivery_mode": "direct_hold",
      "dispatch_mode": "single",
      "max_in_flight": 100,
      "max_replay": 3,
      "ack_after": "target_accept",
      "on_expired": "dlq",
      "on_perm_failure": "dlq",
      "on_filtered": "drop"
    }
  ]
}
```

### Deep health response

Returns HTTP 200 when ready for traffic, HTTP 503 otherwise. Covers runtime
state, session connectivity, lease status, and subscription convergence.

```json
{
  "running": true, "healthy": true,
  "instance_id": "gobridge-1", "role": "standalone",
  "ready_for_traffic": true, "empty": false, "service_level": "full",
  "level": "full",
  "sessions": [{
    "session_id": "mqtt-conn", "connected": true, "has_lease": false,
    "subscriptions_wanted": 2, "subscriptions_active": 2,
    "ready": true, "service_level": "full"
  }],
  "routes": [{ "id": "ingest", "delivery_mode": "direct_hold" }]
}
```

The `service_level` field aggregates across sessions: `full`, `degraded`, `none`.

The `empty` field is true when the instance carries no routes and no sessions,
so no message can be bridged through it. That is what a bridge started without
a usable configuration looks like -- for example when the configured file does
not exist and the process fell back to starting empty. An empty instance stays
alive (`/live` answers 200, so it is not restarted), but it never claims
`ready_for_traffic` and never reaches the `full` readiness level, so a load
balancer or a rollout gate does not steer traffic at a bridge that carries
none. Distinguish it from a bridge whose routes are merely still coming up:
both are not ready, but only the second one becomes ready on its own.

When the deep health response includes a `config_watch` object, its optional
`restart_required` field names a part of the desired configuration this process
has accepted and stored but cannot apply while running -- today, a changed
`http` block, because the listeners are bound once at startup. It stays absent
while the running process matches its desired configuration.

## HTTP Transport Endpoints

The HTTP transport adapter exposes endpoints for message ingestion and SSE
streaming on a shared `http.ServeMux` -- separate from the admin/monitor
servers. See [Transport Configuration](transport-configuration.md) for
options. OpenAPI spec: `spec/http-adapter/http-api.yaml`.

| Direction | Method | Path | Description |
|-----------|--------|------|-------------|
| Ingress | POST | `/transport/http/receivers/{id}/messages` | JSON message with `subject`, `payload`, `headers`, `expires_at` |
| Egress | GET | `/transport/http/senders/{id}/events` | SSE stream with heartbeats and cluster-aware redirect (307) |

Per-endpoint API keys use the same SHA-256 constant-time comparison as the
management API. Both `X-API-Key` and `Authorization: Bearer` are accepted.

