# HTTP API Examples

> Part of the [Credential Management and HTTP API](credentials-and-http-api.md)
> reference. See the [HTTP API Reference](http-api.md) for endpoint details.

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
    # Naming the session on the binding is what makes the bridge manage it:
    # a session nobody manages never connects, and every publish fails.
    session_id: mqtt-tls
    address: events/out

stores:
  dlq:
    type: sqlite
    options:
      path: /var/lib/gobridge/state/dlq.db

routes:
  - id: forward-http
    receiver_id: http-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-mqtt]

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
