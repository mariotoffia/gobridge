# Credential Management and HTTP API Reference

This document covers the GoBridge credential resolution system and the HTTP
admin/monitor API. It is intended for operators deploying GoBridge and
developers integrating with its management endpoints.

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
    ssmcreds.WithRegion("us-east-1"),
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
```

| Field | YAML key | Type | Default | Description |
|-------|----------|------|---------|-------------|
| AdminAddr | `admin_addr` | string | `":8080"` | Admin server listen address |
| MonitorAddr | `monitor_addr` | string | `":8081"` | Monitor server listen address |
| AdminAPIKey | `admin_api_key` | string | *required* | API key for admin endpoints (min 16 chars) |
| MonitorAPIKey | `monitor_api_key` | string | *optional* | Separate key for monitor; falls back to `admin_api_key` |
| CORSOrigins | `cors_origins` | string | `""` | Comma-separated allowed origins; wildcard `*` is rejected |

### Authentication

All authenticated endpoints accept credentials via:

- **Header**: `X-API-Key: <key>`
- **Bearer token**: `Authorization: Bearer <key>`

Key comparison uses constant-time SHA-256 hashing to prevent timing attacks.

### Middleware chain

Every request passes through (in order): request logging, security headers
(`X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`), CORS (when
configured), panic recovery, and correlation ID injection
(`X-Correlation-ID`, `X-Trace-ID`, `X-Span-ID`).

## Admin API Endpoints

Default listen address: `:8080`. All endpoints require authentication.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/bridge` | Instance info (ID, running status, route count) |
| POST | `/api/v1/admin/bridge/start` | Start the bridge |
| POST | `/api/v1/admin/bridge/stop` | Stop the bridge |
| GET | `/api/v1/admin/routes` | List all routes with delivery/dispatch mode |
| POST | `/api/v1/admin/routes/{routeID}/inject` | Inject a test message (base64 payload) |
| GET | `/api/v1/admin/dlq` | List DLQ entries (limit 100) |
| GET | `/api/v1/admin/dlq/messages` | Retrieve DLQ messages (filter by `route_id`, `category`) |
| POST | `/api/v1/admin/dlq/replay` | Replay DLQ entries by ID (max 1000) |
| POST | `/api/v1/admin/dlq/purge` | Purge expired DLQ entries |

### Inject request body

```json
{
  "subject": "devices/sensor-1/data",
  "payload": "eyJoZWxsbyI6IndvcmxkIn0=",
  "headers": { "x-source": "test" }
}
```

The `payload` field is base64-encoded. Reserved headers are stripped
automatically.

## Monitor API Endpoints

Default listen address: `:8081`.

### Unauthenticated (health probes)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/health` | Health check (status, instance ID, failed components) |
| GET | `/api/v1/monitor/live` | Liveness probe (always `{"status":"alive"}`) |
| GET | `/api/v1/monitor/ready` | Readiness probe (includes `role`: standalone/active/standby) |

### Authenticated

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/topology` | Bridge topology graph (routes with delivery/dispatch modes) |
| GET | `/api/v1/monitor/routes` | Route status with policy details (max_in_flight, ack_after, etc.) |
| GET | `/api/v1/monitor/logs` | Recent log entries (not yet implemented) |

## Complete YAML Example

```yaml
bridge:
  id: secure-bridge

sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      broker_url: tls://mqtt.example.com:8883
      client_id: bridge-secure
      credentials_uri: file://prod/mqtt/broker-creds

senders:
  - id: sqs-out
    transport: sqs
    options:
      queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/events
      credentials_uri: pms://prod/aws/sqs-creds

bindings:
  - id: to-sqs
    sender_id: sqs-out
    address: events

receivers:
  - id: mqtt-in
    session_id: mqtt-tls
    topics:
      - topic: "events/#"
        qos: 1

routes:
  - id: forward
    receiver_id: mqtt-in
    bindings: [to-sqs]

http:
  admin_addr: ":8080"
  monitor_addr: ":8081"
  admin_api_key: "change-me-to-a-real-key"
  monitor_api_key: ""
  cors_origins: "https://dashboard.example.com"
```

## Programmatic Setup

```go
package main

import (
    "context"
    "log/slog"

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
        ssmcreds.WithRegion("us-east-1"),
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
