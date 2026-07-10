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

## Resolver caching and failure behavior

Every resolve path funnels through one fetch choke point in the
`CredentialResolver`: the build-time synchronous resolve, the uncached rotation
polls (see [credentials-rotation.md](credentials-rotation.md)), and the reactive
re-resolve after an auth failure. That choke point owns credential-resolve
observability and the stale-while-error policy.

On any repository error the resolver emits `CredentialResolveFailure` tagged
with the bounded error `code` (`NOT_AUTHORIZED`, `UNAVAILABLE`, `NOT_FOUND`, …),
so a permission denial is distinguishable from a backend outage.

**Stale-while-error.** On a *retryable* (transient) error -- throttled, timeout,
unavailable -- the resolver serves the last-known-good but **expired** cached
credential instead of failing the build/rebuild, logs a WARN, and emits
`CredentialStaleServed`. The stale entry's TTL is not extended, so the next
resolve re-probes the backend and recovery is immediate once it returns.
*Permanent* errors (`NOT_FOUND`, `INVALID_PAYLOAD`, `NOT_AUTHORIZED`) always
propagate -- stale credentials never mask a revocation or a misconfiguration.

Operator consequence: a credential **rebuild** survives a bounded secrets-backend
outage on last-known-good material, while a **revocation** still propagates the
moment the backend recovers and returns the permanent error. Stale-serving needs
the cache; it does not apply when caching is disabled
(`WithCredentialCacheTTL(0)`).

| Metric | Dimensions | Meaning |
|--------|-----------|---------|
| `CredentialResolveFailure` | `code` | Repository fetch failed at the resolver (any resolve path). |
| `CredentialStaleServed` | `code` | Served an expired last-known-good credential after a retryable error. |
| `CredentialRotationApplied` | none | A rotation was applied to a live transport target. |

Full dimensions, units, and alarm guidance live in
[AWS monitoring](aws-deployment/monitoring.md#key-metrics).

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

### Stock binary wiring

The stock `gobridge` binary registers the `file://` repository unconditionally,
driven by the `-credentials-dir` flag (default `credentials`), so `file://`
credential URIs in a config resolve out of the box. A file-store init failure --
for example a read-only working directory where the base dir does not already
exist -- is not fatal: `file://` URIs then fail at resolve time with a clear
error, but a config that uses only `pms://` (or no) credentials still boots.
In the AWS file-based deployment the same wiring is opt-in via
`credential_file_path` (see [AWS configuration](aws-deployment/configuration.md#field-reference));
`pms://` (SSM) is always registered there.

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
>
> Read-only and immutable mounts are tolerated: a Kubernetes Secret mounted
> read-only does not crash-loop the store. When the base directory already
> exists the repository accepts it, and the 0700 permission tighten is
> best-effort -- a chmod that fails because the mount is read-only or unowned
> (EROFS/EPERM) is logged at WARN and tolerated. See the
> [Kubernetes secret-mount cookbook](scenarios/22-k8s-secret-mount-credentials.md).

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
2. **JSON opaque `secret`** -- a password-only credential with **no username**,
   declared by a `"secret"` type token (aliases `"opaque"`, `"sas"`) with the
   whole value in a `secret` (or `password`) field. This is the shape an Azure
   Service Bus SAS connection string takes at runtime
   (`PasswordCredential{Username:"", Password:<connection string>}`). The opaque
   shape is selected **only when the document carries no `username`/`user`
   field**; a secret/opaque/sas token that appears *alongside* a username parses
   as an ordinary username/password credential instead. That preserves the
   behaviour of readers predating the opaque shape (which ignored the token and
   parsed such documents as username/password), so credential sets already
   stored in SSM stay readable after upgrade. The secret value must be
   non-blank. Example:
   `{"type":"secret","secret":"Endpoint=sb://…;SharedAccessKeyName=…;SharedAccessKey=…"}`.
3. **JSON with TLS fields** -- detected by `certPem` or `cert` key presence. A
   CA-only bundle (server verification, no mutual TLS) is accepted; a
   `certPem`/`keyPem` field that is present but whitespace-only is normalised to
   empty so it never reaches a transport as a blank-but-non-empty key pair.
4. **Simple format** -- `username:password` plain text (both parts required;
   opaque secrets must use the JSON `secret` shape above, since a connection
   string itself contains colons).

A `type` field (`"password"`, `"secret"`, `"tls"`, `"certificate"`, or a
`+`-combined token such as `"password+tls"`) can disambiguate when more than one
key set is present.

### Write validation (Create / Update)

The admin write path serializes as JSON and then **round-trips the payload back
through the reader** before calling `PutParameter`. A write that would produce a
value the reader cannot parse — an empty set, a torn TLS half (a certificate
without its key, or vice-versa), an empty password, or a **whitespace-only
opaque secret** — is rejected with `shared.ErrInvalidPayload` and never reaches
SSM. This guarantees write format == read format, so a single bad admin write
can never persist a value that every later `Get` or rotation poll would fail to
parse (a self-inflicted credential outage).

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

