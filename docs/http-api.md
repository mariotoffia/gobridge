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
[Audit logging](http-api-admin.md#audit-logging)).

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

The admin endpoint reference — config transactions, DLQ, credentials — is on its own page: [Admin API endpoints](http-api-admin.md).

## Monitor API Endpoints

The monitor server and its probes are documented in
[Monitor API Endpoints](http-api-monitor.md).

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
