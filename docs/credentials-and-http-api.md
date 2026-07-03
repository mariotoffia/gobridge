# Credential Management and HTTP API Reference

This document covers the GoBridge credential resolution system. The HTTP
admin/monitor API reference now lives in [HTTP API Reference](http-api.md) and
[HTTP API Examples](http-api-examples.md) (linked at the end). It is intended
for operators deploying GoBridge and developers integrating with its management
endpoints.

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
      session:
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

## HTTP API

The HTTP admin/monitor API and transport-endpoint reference now live in
dedicated documents:

- [HTTP API Reference](http-api.md) -- server configuration, admin and monitor
  API endpoints, and HTTP transport endpoints.
- [HTTP API Examples](http-api-examples.md) -- a complete end-to-end YAML
  example and the programmatic setup.

