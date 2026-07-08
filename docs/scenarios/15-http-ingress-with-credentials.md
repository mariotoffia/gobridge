# Scenario 15: HTTP Ingress with Credential-Based TLS Egress

Accept webhook messages over HTTP and forward them to an MQTT broker secured with TLS client certificates, using credential URIs for secret management and API keys for HTTP endpoint protection.

## Use Case

A SaaS platform receives webhook events from external partners via HTTP POST. These events must be forwarded to an internal MQTT broker that requires mutual TLS (mTLS) authentication. You want to:

1. **Protect the HTTP ingress** with an API key so only authorized partners can send events.
2. **Store MQTT TLS credentials** outside the config file using the credential URI system.
3. **Manage the bridge** through the HTTP admin API with a separate management key.
4. **Monitor health** through unauthenticated probes for your load balancer.

This scenario demonstrates the two-layer security model: **API keys** protect HTTP endpoints at request time, while **credential URIs** resolve transport authentication material at build time.

## Architecture

```mermaid
flowchart LR
    subgraph "External"
        P["Partner\nWebhook"] -->|"POST + X-API-Key"| HTTP["HTTP Receiver\nwebhook-in"]
    end

    subgraph "GoBridge"
        HTTP --> Route["Route\nforward"]
        Route --> Binding["Binding\nto-mqtt"]
        CR["Credential\nResolver"] -.->|"TLS certs\nat build time"| Session
    end

    subgraph "Internal"
        Binding --> Session["MQTT Session\n(mTLS)"]
        Session --> Broker["MQTT Broker\ntls://mqtt.internal:8883"]
    end

    subgraph "Operations"
        OP["Operator"] -->|"Admin API\n+ admin key"| Admin[":8080\nAdmin"]
        LB["Load Balancer"] -->|"Health probes"| Monitor[":8081\nMonitor"]
    end

    style Route fill:#f96,stroke:#333
    style CR fill:#9cf,stroke:#333
    style Admin fill:#6cf,stroke:#333
```

## Credential File

Store the MQTT broker credentials in a JSON file with restricted permissions (0600):

```json
{
  "credentials": {
    "Password": {
      "Username": "bridge-service",
      "Password": "broker-s3cret"
    },
    "TLS": {
      "CertPEM": "-----BEGIN CERTIFICATE-----\nMIIC...\n-----END CERTIFICATE-----",
      "KeyPEM": "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----",
      "CAPEMs": [
        "-----BEGIN CERTIFICATE-----\nMIID...\n-----END CERTIFICATE-----"
      ],
      "InsecureSkipVerify": false
    }
  },
  "version": 1,
  "createdAt": "2026-01-15T10:00:00Z",
  "updatedAt": "2026-01-15T10:00:00Z"
}
```

Save this as `/etc/gobridge/creds/prod/mqtt/broker-creds.json`. The credential resolver maps URI `file://prod/mqtt/broker-creds` to this file path.

## Configuration

```yaml
bridge:
  id: webhook-to-mqtt

sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      credentials_uri: file://prod/mqtt/broker-creds
      session:
        broker_url: tls://mqtt.internal:8883
        client_id: webhook-bridge-01

receivers:
  - id: webhook-in
    transport: http
    options:
      path: /webhooks/events
      api_key: "partner-webhook-key-min16"
      max_body_size: 524288

senders:
  - id: mqtt-out
    session_id: mqtt-tls

bindings:
  - id: to-mqtt
    sender_id: mqtt-out
    address: "webhooks/{source}"

routes:
  - id: forward
    receiver_id: webhook-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-mqtt]
    policy:
      max_in_flight: 200
      max_replay_attempts: 3
      on_permanent_failure: dlq

stores:
  dlq:
    type: memory

http:
  admin_addr: ":8080"
  monitor_addr: ":8081"
  admin_api_key: "admin-mgmt-key-min-16-chars"
  monitor_api_key: "monitor-readonly-key-16ch"
  cors_origins: "https://ops-dashboard.example.com"
```

## Config Walkthrough

### Credential URI: `credentials_uri: file://prod/mqtt/broker-creds`

At build time, the `CredentialResolver`:

1. Matches scheme `file` to the registered file credential repository.
2. Resolves `file://prod/mqtt/broker-creds` to `/etc/gobridge/creds/prod/mqtt/broker-creds.json`.
3. Reads the `PasswordCredential` and `TLSMaterial` from the JSON file.
4. Merges `username`, `password`, `tls_cert`, `tls_key`, and `tls_ca` into the session options.
5. Removes the `credentials_uri` key before the MQTT transport sees the config.

The transport receives options that look like inline credentials, but the actual secrets never appear in the YAML file.

### HTTP Receiver: `api_key: "partner-webhook-key-min16"`

Partners must include the key in every request:

```bash
curl -X POST -H "X-API-Key: partner-webhook-key-min16" \
  -H "Content-Type: application/json" \
  -d '{"subject":"orders/new","payload":{"order_id":"12345"},"headers":{"source":"partner-a"}}' \
  http://bridge.example.com/webhooks/events
```

The key is compared using SHA-256 constant-time hashing. Invalid or missing keys receive HTTP 401. The `max_body_size` (512 KiB) limits request size.

### Management API Keys

Three separate keys protect different access levels:

| Key | Protects | Access Level |
|-----|----------|-------------|
| `admin_api_key` | Admin endpoints (`:8080`) | Bridge control, DLQ management, message injection |
| `monitor_api_key` | Monitor authenticated endpoints (`:8081`) | Topology, route details, deep health |
| `api_key` (receiver) | HTTP ingress endpoint | Message ingestion |

The admin key also grants monitor access (superset). Health probes (`/health`, `/live`, `/ready`) are always unauthenticated for load balancer compatibility.

### Address Template: `webhooks/{source}`

The binding address uses `{source}` as a template placeholder. When the partner sends `"headers": {"source": "partner-a"}`, the MQTT publish topic resolves to `webhooks/partner-a`. Missing headers cause an error.

## Security Layers

```mermaid
sequenceDiagram
    participant P as Partner
    participant R as HTTP Receiver
    participant CR as Credential Resolver
    participant M as MQTT Session

    Note over CR: Build time -- credential resolution
    CR->>CR: Resolve file://prod/mqtt/broker-creds
    CR->>M: Inject username, password, TLS certs
    M->>M: Connect with mTLS to broker

    Note over P,R: Runtime -- request authentication
    P->>R: POST /webhooks/events + X-API-Key
    R->>R: SHA-256 hash + constant-time compare
    alt valid key
        R->>M: Route envelope to MQTT sender
        M->>M: Publish to tls://mqtt.internal:8883
        M-->>R: ACK
        R-->>P: 200 {"status":"accepted"}
    else invalid key
        R-->>P: 401 + WWW-Authenticate header
    end
```

## Go Bootstrap

```go
package main

import (
    "context"
    "net/http"
    "log/slog"
    "os"

    "github.com/mariotoffia/gobridge/bridge"
    "github.com/mariotoffia/gobridge/config"
    "github.com/mariotoffia/gobridge/httpapi"
    "github.com/mariotoffia/gobridge/runtime"
    adaptershttp "github.com/mariotoffia/gobridge/adapters/http/transport"
    filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"
    nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
    "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto)

    // Credential repository for file:// URIs
    fileRepo, _ := filecreds.New("/etc/gobridge/creds",
        filecreds.WithNamespace("prod"),
    )
    resolver := runtime.NewCredentialResolver()
    resolver.Register(fileRepo)

    // HTTP transport factory
    httpFactory := adaptershttp.NewBridgeFactory(
        adaptershttp.WithFactoryLogger(logger),
    )

    // Build runtime
    rt, _ := bridge.NewBuilder(cfg,
        bridge.WithCredentialStore(resolver),
        bridge.WithLogger(logger),
    ).
        RegisterTransport("mqtt", paho.NewFactory(logger)).
        RegisterTransport("http", httpFactory).
        RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
        Build(context.Background())

    // Mount HTTP transport endpoints (webhook receiver)
    go func() {
        mux := http.NewServeMux()
        mux.Handle("/webhooks/", httpFactory.Handler())
        _ = http.ListenAndServe(":9090", mux)
    }()

    // Start admin/monitor API servers
    apiServer := httpapi.New(rt, httpapi.Config{
        AdminAddr:     cfg.HTTP.AdminAddr,
        MonitorAddr:   cfg.HTTP.MonitorAddr,
        AdminAPIKey:   cfg.HTTP.AdminAPIKey,
        MonitorAPIKey: cfg.HTTP.MonitorAPIKey,
        CORSOrigins:   cfg.HTTP.CORSOrigins,
    }, httpapi.WithServerLogger(logger))

    ctx := context.Background()
    apiServer.Start(ctx)
    rt.Start(ctx)
    // ... wait for signal ...
    rt.Stop(ctx)
    apiServer.Stop(ctx)
}
```

## Variations

### AWS SSM Instead of File Credentials

Replace the file backend with SSM Parameter Store for cloud-native deployments:

```yaml
sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      credentials_uri: pms://prod/mqtt/broker-creds
      session:
        broker_url: tls://mqtt.internal:8883
        client_id: webhook-bridge-01
```

```go
import ssmcreds "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"

ssmRepo := ssmcreds.New(
    ssmcreds.WithRegion("us-west-1"),
    ssmcreds.WithNamespace("prod"),
)
resolver.Register(ssmRepo)
```

SSM stores credentials as `SecureString` parameters encrypted with KMS.

### Mixed Credential Backends

Use file credentials for local development and SSM for production. Both repositories can be registered simultaneously -- the resolver dispatches by URI scheme:

```go
resolver.Register(fileRepo)  // handles file:// URIs
resolver.Register(ssmRepo)   // handles pms:// URIs
```

```yaml
# Dev: file://dev/mqtt/creds (local JSON file)
# Prod: pms://prod/mqtt/creds (AWS SSM)
sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      credentials_uri: file://dev/mqtt/creds  # switch to pms:// in prod
      session:
        broker_url: tls://mqtt.internal:8883
```

### HTTP Receiver with SSE Fan-Out

Add an SSE sender to stream webhook events to a monitoring dashboard:

```yaml
senders:
  - id: mqtt-out
    session_id: mqtt-tls
  - id: sse-monitor
    transport: http
    options:
      mode: sse
      path: /monitor/webhooks
      api_key: "dashboard-key-min-16-ch"
      max_clients: 50

bindings:
  - id: to-mqtt
    sender_id: mqtt-out
    address: "webhooks/{source}"
  - id: to-dashboard
    sender_id: sse-monitor
    address: webhooks

routes:
  - id: forward
    receiver_id: webhook-in
    delivery_mode: direct_hold
    dispatch_mode: fan_out
    bindings: [to-mqtt, to-dashboard]
```

This fans out every webhook to both the MQTT broker (with mTLS) and a real-time SSE stream (with its own API key).

### Credential Cache Tuning

The default cache TTL is 5 minutes with up to 1000 entries. For long-running bridges with stable credentials, increase the TTL to reduce file/SSM lookups:

```go
resolver := runtime.NewCredentialResolver(
    runtime.WithCredentialCacheTTL(30 * time.Minute),
)
```

For short-lived credentials (e.g., rotating tokens), reduce the TTL or disable caching:

```go
resolver := runtime.NewCredentialResolver(
    runtime.WithCredentialCacheTTL(0), // disable cache
)
```

The cache also backs **stale-while-error**: on a transient secrets-backend error
the resolver serves the last-known-good (expired) credential and increments
`CredentialStaleServed` instead of failing the build, then re-probes on the next
resolve. Disabling the cache (`WithCredentialCacheTTL(0)`) turns this off -- a
transient backend blip then fails the rebuild. Permanent errors (`NOT_FOUND`,
`NOT_AUTHORIZED`) always propagate. See
[Credentials & HTTP API](../credentials-and-http-api.md#resolver-caching-and-failure-behavior)
and, for backing `file://` with a mounted Kubernetes Secret, the
[Kubernetes secret-mount cookbook](22-k8s-secret-mount-credentials.md).

## Notes

- **Credential values are never logged.** The `domain.PasswordCredential` and `domain.TLSMaterial` types intentionally have no `String` or `GoString` methods.
- **Inline options take precedence.** If `username` already exists in the session options map, the credential resolver does not overwrite it.
- **Credential resolution happens once at build time.** The resolver cache is for repeated builds or hot-reload scenarios. Once the runtime is built, credentials are baked into transport configs.
- **API keys and credential URIs are independent.** A receiver can use an API key without any credential URI, and a session can use a credential URI without any API key. They solve different problems at different layers.
