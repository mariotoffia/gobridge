# Credential Management and HTTP API Reference

This document covers the GoBridge credential resolution system and the HTTP
admin/monitor API. It is intended for operators deploying GoBridge and
developers integrating with its management endpoints.

> **OpenAPI specifications** are available under `spec/httpapi/` (admin +
> monitor) and `spec/http-adapter/` (HTTP transport). See those files for
> machine-readable endpoint definitions.

> **Runtime rotation** -- this document describes how credentials are
> resolved when the bridge builds its transports. See
> [credentials-rotation.md](credentials-rotation.md) for the
> complementary story of how GoBridge rotates credentials on a
> *running* bridge (Pull vs Push stores, the `CredentialRefresher`,
> the `CredentialAware` capability, and per-transport behaviour).

## Credential URI System Overview

Sessions, receivers, senders, and bindings accept a `credentials_uri` key in
their `options` map. During build time the `Builder` resolves each URI through
registered `CredentialRepository` backends, merges the resulting material into
transport options, and removes the `credentials_uri` key before the transport
sees the configuration.

Resolution uses **scheme dispatch** -- the URI scheme (`file`, `pms`) selects
matching repositories -- combined with **longest namespace prefix matching**
when several repositories share the same scheme. The `CredentialResolver`
caches resolved credentials with a configurable TTL (default 5 minutes, max
1000 entries).

```mermaid
sequenceDiagram
    participant B as Builder
    participant CR as CredentialResolver
    participant R as Repository (file / pms)
    participant T as Transport Options

    B->>CR: Resolve("file://prod/mqtt/creds")
    CR->>CR: Check cache (TTL-based)
    alt cache miss
        CR->>CR: Match scheme "file"
        CR->>CR: Longest namespace prefix match
        CR->>R: Get("file://prod/mqtt/creds")
        R-->>CR: CredentialSet{Password, TLS}
        CR->>CR: Store in cache
    end
    CR-->>B: CredentialSet
    B->>T: Merge password into username/password
    B->>T: Merge TLS into tls_cert/tls_key/tls_ca
    B->>T: Delete credentials_uri key
```

## `file://` Backend

The file backend stores credentials as versioned JSON files on disk.

| Property | Value |
|----------|-------|
| **Scheme** | `file` |
| **URI format** | `file://namespace/path/to/creds` |
| **Disk path** | `basePath/namespace/path/to/creds.json` |
| **File mode** | `0600` |
| **Package** | `adapters/native/credentials/file` |

### File structure

```json
{
  "credentials": {
    "Password": {
      "Username": "mqtt-user",
      "Password": "s3cret"
    },
    "TLS": {
      "CertPEM": "-----BEGIN CERTIFICATE-----...",
      "KeyPEM": "-----BEGIN PRIVATE KEY-----...",
      "CAPEMs": ["-----BEGIN CERTIFICATE-----..."],
      "InsecureSkipVerify": false
    }
  },
  "version": 1,
  "createdAt": "2026-01-15T10:00:00Z",
  "updatedAt": "2026-01-15T10:00:00Z"
}
```

### Repository setup

```go
import filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"

repo, err := filecreds.New("/etc/gobridge/creds",
    filecreds.WithNamespace("prod"),
)
```

`New` returns an error if `basePath` is empty or the directory cannot be
created. The repository also implements `ports.CredentialAdmin` for
Create/Update/Delete/List operations with optimistic version locking.

### Durability and safe writes

Create/Update write through a temp file in the destination directory (`0600`
from creation), `fsync` it, atomically `rename` it over the target, then
`fsync` the directory. A crash mid-write or a concurrent external reader can
therefore only ever observe the complete previous file or the complete new
file — never a truncated or partially written secret. Write/parse/IO failures
surface as classified `shared.BridgeError` values, and all operations honour a
cancelled `context`.

> **Production posture.** The `file://` backend is suited to local/dev use and
> to immutable mounted secrets. It is single-process (an in-process
> `RWMutex` serialises its own readers/writers); it is not a multi-writer or
> multi-process rotating secret store. For rotating secrets in production use
> the `pms://` (SSM) or Secrets Manager backends.

## `pms://` Backend (AWS SSM Parameter Store)

The SSM backend stores credentials as `SecureString` parameters encrypted at
rest with KMS.

| Property | Value |
|----------|-------|
| **Scheme** | `pms` |
| **URI format** | `pms://namespace/path/to/parameter` |
| **SSM path** | `/namespace/path/to/parameter` |
| **Storage type** | `SecureString` |
| **Package** | `adapters/aws/credentials/ssm` |

### Supported formats (read)

1. **JSON with `username`/`password` fields** -- detected by `username` or
   `user` key presence.
2. **JSON with TLS fields** -- detected by `certPem` or `cert` key presence.
3. **Simple format** -- `username:password` plain text.

A `type` field (`"password"`, `"tls"`, `"certificate"`) can disambiguate when
both key sets are present.

### Repository setup

```go
import ssmcreds "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"

repo := ssmcreds.New(
    ssmcreds.WithRegion("us-west-1"),
    ssmcreds.WithNamespace("prod"),
)
```

| Option | Description |
|--------|-------------|
| `WithClient(ssmAPI)` | Pre-configured or mock SSM client |
| `WithRegion(region)` | AWS region for lazy client construction |
| `WithEndpoint(endpoint)` | Custom SSM endpoint (e.g. LocalStack) |
| `WithProfile(profile)` | AWS shared-config profile name |
| `WithNamespace(namespace)` | Namespace prefix for dispatch matching |

The SSM client is built lazily on first use unless `WithClient` is provided.

## Resolved Credential Types

A `CredentialSet` may contain either or both of the following. Nil fields
indicate that credential kind is not present.

### PasswordCredential

| Field | Go type | Merged option key |
|-------|---------|-------------------|
| `Username` | `string` | `username` |
| `Password` | `string` | `password` |

### TLSMaterial

| Field | Go type | Merged option key |
|-------|---------|-------------------|
| `CertPEM` | `string` | `tls_cert` |
| `KeyPEM` | `string` | `tls_key` |
| `CAPEMs` | `[]string` | `tls_ca` |
| `InsecureSkipVerify` | `bool` | `tls_insecure` |

Inline option values take precedence. If `username` already exists in options,
the resolved value is not applied. This lets operators override individual
fields while still using URI-based resolution for the rest.

## Combining Credential URIs with API Keys

Credential URIs and API keys serve different purposes and can be used together:

- **Credential URIs** (`credentials_uri`) resolve **transport authentication**
  -- broker passwords, TLS certificates, cloud service tokens. They are
  resolved at build time and merged into transport options.
- **API keys** (`api_key`, `admin_api_key`, `monitor_api_key`) protect
  **HTTP endpoints** -- the admin/monitor management API and individual HTTP
  transport receivers/senders. They are validated at request time.

A typical production deployment uses both:

```yaml
sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      broker_url: tls://mqtt.example.com:8883
      credentials_uri: file://prod/mqtt/broker-creds   # transport auth

receivers:
  - id: http-in
    transport: http
    options:
      api_key: "ingress-key-min-16-chars"               # HTTP endpoint auth

http:
  admin_api_key: "admin-key-min-16-characters"           # management API auth
  monitor_api_key: "monitor-key-min-16-chars"            # monitor API auth
```

The credential URI resolves the MQTT broker username/password and TLS material
at build time. The API keys protect runtime HTTP endpoints independently.

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

TLS is opt-in and applies to **both** the admin and monitor listeners. When
both `tls_cert_file` and `tls_key_file` are present the servers serve HTTPS and
the reported `AdminURL`/`MonitorURL` use the `https` scheme; when both are empty
the servers stay plaintext (the historical default) on the assumption that an
external terminator (LB/ingress/mesh) provides TLS. Supplying exactly one of the
pair is rejected at startup.

### Authentication failure throttling

Both the admin and monitor auth middlewares throttle repeated failures **per
client**. After `AuthFailureLimit` failed attempts within `AuthFailureWindow`,
further attempts from that client are rejected with **HTTP 429** and a
`Retry-After: 60` header until the window rolls over; a successful auth resets
the client's counter. These knobs live on the programmatic `httpapi.Config`
(they are not part of the YAML `http:` block):

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `AuthFailureLimit` | int | `5` | Failed attempts per client per window before 429 |
| `AuthFailureWindow` | duration | `1m` | Fixed window over which failures are counted |

The client identity used for both throttling and audit attribution is derived
by `actorFromRequest`: the leftmost `X-Forwarded-For` hop when present,
otherwise `RemoteAddr`. **`X-Forwarded-For` is client-spoofable** unless a
trusted edge proxy overwrites it, so deployments MUST terminate/normalise XFF at
a trusted proxy for this attribution to be authoritative (and to prevent a
spoofed XFF from evading or poisoning the throttle).

### Authentication

All authenticated endpoints accept credentials via:

- **Header**: `X-API-Key: <key>`
- **Bearer token**: `Authorization: Bearer <key>`

Key comparison uses **SHA-256 hashing followed by constant-time comparison**
(`crypto/subtle.ConstantTimeCompare`) to prevent both timing attacks and
length-based information leaks. Failed authentication returns HTTP 401 with a
`WWW-Authenticate: Bearer realm="gobridge-admin"` (or `gobridge-monitor`)
header per RFC 9110.

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
| POST | `/api/v1/admin/bridge/start` | Start the bridge (`AdminOperationTimeout`, default 30s) |
| POST | `/api/v1/admin/bridge/stop` | Stop the bridge (`AdminOperationTimeout`, default 30s) |
| GET | `/api/v1/admin/routes` | List all routes with delivery/dispatch mode |
| POST | `/api/v1/admin/routes/{routeID}/inject` | Inject a test message (JSON payload, 1 MB limit) |
| GET | `/api/v1/admin/dlq` | DLQ summary (`configured`, `count`, `count_capped`) |
| GET | `/api/v1/admin/dlq/messages` | Paginated DLQ messages (filter by `route_id`, `category`, `since`, `before`) |
| GET | `/api/v1/admin/dlq/messages/{id}` | Single DLQ entry with full payload (audited as `dlq.read_payload`) |
| POST | `/api/v1/admin/dlq/redrive` | Redrive DLQ entries by ID (max 100); 207 on partial failure |
| POST | `/api/v1/admin/dlq/delete` | Delete DLQ entries by ID (max 1000) |
| POST | `/api/v1/admin/dlq/delete-by-filter` | Delete by filter (requires `confirm_delete_all` for an empty filter) |
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

The `payload` field is base64-encoded. Reserved `X-Bridge-*` headers are
stripped automatically. Request body limited to 1 MB.

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
**claim-by-delete before inject**: the entry is deleted first so a client retry
or a concurrent admin instance cannot double-deliver (a crash between delete and
inject accepts an at-most-once drop; an inject failure best-effort restores the
entry). Replay is **binding-scoped** -- the entry's originating `binding_id` is
pinned via a route-override header so a fan-out route re-delivers only to the
binding that failed, not to its healthy siblings.

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

Commit refuses to erase plugin options: if the merged config would drop the
typed `Config` of any entry that previously had one -- most commonly by changing
a `transport`/`type` discriminator via PATCH (which cannot carry replacement
options) -- the commit fails with **HTTP 422** and `errConfigOptionsLoss`
("config commit would erase plugin options"). Other outcomes: `404`
(transaction not found/expired), `409` (version conflict / concurrent write),
`422` (config validation failed, with `validation_errors`).

Commit is **write-then-apply**. On success it returns
`{"status": "committed", "version": N}`. When a `ConfigApplier` is wired and the
durable write succeeds but the in-band apply fails, commit returns **HTTP 500**
with `{"status": "committed_not_applied", "version": N, ...}` -- disk and the
running runtime have diverged and an operator must reconcile (the version is on
disk).

### Audit logging

All admin endpoints emit audit events with timestamp, action, actor, resource
type, resource ID, outcome (`success` / `failure` / `partial_failure`), and an
optional detail map. The **actor** is derived by `actorFromRequest`: the
leftmost `X-Forwarded-For` hop when present (so a shared edge proxy does not
collapse every operator to the LB address), else `RemoteAddr`. This is only
authoritative when a trusted proxy normalises `X-Forwarded-For`.

Notable actions include `bridge.status`, `bridge.start`, `bridge.stop`,
`route.inject`, `dlq.redrive`, `dlq.purge`, `config.txn.patch` /
`config.txn.commit` / `config.txn.rollback`, and -- new in this release:

| Action | Emitted when |
|--------|--------------|
| `auth.failure` | An API key check fails (also increments the per-client throttle) |
| `auth.throttled` | A request is rejected with 429 because the client is over its failure limit |
| `dlq.read_payload` | A single DLQ entry (with full payload) is read via `GET .../dlq/messages/{id}` |

## Monitor API Endpoints

Default listen address: `:8081`.

### Unauthenticated (health probes)

All health probes set `Cache-Control: no-cache, max-age=0` to prevent stale
caching by load balancers and CDNs.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/health` | Coarse health check -- returns **only** `{"status": ...}` |
| GET | `/api/v1/monitor/live` | Liveness probe (`{"status":"alive"}`, or `{"status":"terminal"}` + 503) |
| GET | `/api/v1/monitor/ready` | Readiness probe (`{"status":"ready","role":...}` or 503) |

The `health` endpoint is **unauthenticated** (for load balancers/orchestrators)
and therefore exposes only a coarse `status` string plus the HTTP status code --
never `instance_id`, route count, or component-failure detail, which are
reconnaissance and live behind auth on `/deephealth`. It returns HTTP 200 with
`{"status":"ok"}` when healthy, and HTTP 503 with `{"status": ...}` otherwise,
where `status` is one of `ok`, `unhealthy`, `not_running`, or `unavailable`
(runtime not wired).

### Authenticated

Sensitive endpoints require the monitor API key or fall back to the admin key
(admin access is a superset of monitor access).

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/topology` | Bridge topology (instance ID, running state, compact route list) |
| GET | `/api/v1/monitor/routes` | Route details with full policy (max_in_flight, ack_after, on_expired, on_perm_failure) |
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
      "on_perm_failure": "dlq"
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
  "ready_for_traffic": true, "service_level": "full",
  "sessions": [{
    "session_id": "mqtt-conn", "connected": true, "has_lease": false,
    "subscriptions_wanted": 2, "subscriptions_active": 2,
    "ready": true, "service_level": "full"
  }],
  "routes": [{ "id": "ingest", "delivery_mode": "direct_hold" }]
}
```

The `service_level` field aggregates across sessions: `full`, `degraded`, `none`.

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

## Complete YAML Example

```yaml
bridge:
  id: secure-bridge

sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      session:
        client_id: bridge-secure
        broker_urls: ["tls://mqtt.example.com:8883"]
      credentials_uri: file://prod/mqtt/broker-creds

receivers:
  - id: http-in
    transport: http
    options:
      api_key: "http-ingress-api-key-16"

senders:
  - id: mqtt-out
    session_id: mqtt-tls
    options:
      sender:
        default_topic: events/out
        qos: 1

  - id: sqs-out
    transport: sqs
    options:
      queue_url: https://sqs.us-west-1.amazonaws.com/123456789012/events
      credentials_uri: pms://prod/aws/sqs-creds

bindings:
  - id: to-mqtt
    sender_id: mqtt-out
    address: events/out

  - id: to-sqs
    sender_id: sqs-out
    address: events

routes:
  - id: forward-http
    receiver_id: http-in
    dispatch_mode: all
    bindings: [to-mqtt, to-sqs]

http:
  admin_addr: ":8080"
  monitor_addr: ":8081"
  admin_api_key: "change-me-to-a-real-key"
  monitor_api_key: "monitor-readonly-key-16"
  cors_origins: "https://dashboard.example.com"
```

This configuration ingests over HTTP and fans out to an MQTT (TLS) sender and
an SQS sender, demonstrating:
- **Credential URI** on the MQTT session (`file://`) and SQS sender (`pms://`)
  for transport-level authentication. The URI is a top-level `options` key
  (sibling of the nested `session:` / `sender:` role blocks).
- **API key** on the HTTP receiver for endpoint-level protection (minimum 16
  characters).
- **Separate admin and monitor keys** for management API access control (each
  must be at least 16 characters).
- **CORS** restricted to a specific dashboard origin (a literal `*` wildcard is
  rejected).

> MQTT transport options nest under `session:` / `sender:`. MQTT is used here on
> the **egress** (sender) side; an MQTT *receiver* would additionally require a
> connection option on every receiver and subscription entry (see the
> [Transport Configuration](transport-configuration.md) guide).

## Programmatic Setup

```go
package main

import (
    "context"
    "log/slog"
    "time"

    filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"
    ssmcreds  "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"
    "github.com/mariotoffia/gobridge/bridge"
    "github.com/mariotoffia/gobridge/runtime"
)

func main() {
    // Create credential repositories
    fileRepo, err := filecreds.New("/etc/gobridge/creds",
        filecreds.WithNamespace("prod"),
    )
    if err != nil {
        panic(err)
    }

    ssmRepo := ssmcreds.New(
        ssmcreds.WithRegion("us-west-1"),
        ssmcreds.WithNamespace("prod"),
    )

    // Build the resolver and register backends
    resolver := runtime.NewCredentialResolver(
        runtime.WithCredentialCacheTTL(10 * time.Minute),
    )
    resolver.Register(fileRepo)
    resolver.Register(ssmRepo)

    // Wire into the bridge builder
    b := bridge.NewBuilder(cfg,
        bridge.WithCredentialStore(resolver),
        bridge.WithLogger(slog.Default()),
    )

    rt, err := b.Build(context.Background())
    if err != nil {
        panic(err)
    }
    _ = rt
}
```

The resolver dispatches each `credentials_uri` to the repository whose scheme
matches and whose namespace is the longest prefix of the URI path. If no
repository matches, resolution returns a `domain.ErrNotFound` error and the
build fails.
