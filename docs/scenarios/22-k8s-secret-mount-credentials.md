# Scenario 22: Kubernetes Secret-Mount Credentials

Back `file://` transport credentials with a read-only Kubernetes Secret volume.
The `file://` store reads the mounted JSON envelope, the poll wrapper detects a
rotated Secret within one poll interval, and the refresher applies the new
material to live sessions without a restart. This is the on-cluster counterpart
to the AWS SSM (`pms://`) path.

## Use Case

You run GoBridge on Kubernetes and keep broker passwords and TLS material in a
`Secret` (managed directly, by External Secrets, or by a CSI driver). You want
the bridge to read those secrets from a read-only mount, pick up a rotation
without a pod restart, and never crash-loop because the mount is immutable.

## Architecture

```mermaid
flowchart LR
    subgraph Pod["GoBridge Pod"]
        Vol[(Secret volume<br/>read-only, 0400)]
        FR["file:// Repository"]
        CR["CredentialResolver<br/>+ poll wrapper"]
        Sess["MQTT Session<br/>CredentialAware"]
        Vol -->|"reads .json"| FR
        FR -->|"Resolve / poll"| CR
        CR -->|"rotation"| Sess
    end
    Secret[(Kubernetes Secret)] -->|"kubelet sync<br/>atomic ..data swap"| Vol
    Sess -->|"mTLS + password"| Broker[(MQTT Broker)]

    style CR fill:#f96,stroke:#333
```

The operator (or External Secrets) writes the `Secret`; kubelet projects it onto
the volume; the bridge reads and rotates. Nothing writes back through the mount.

## Configuration

The bridge config references the credential by URI. The mount path and the URI
namespace together decide the on-disk file location.

```yaml
bridge:
  id: k8s-secret-mount

sessions:
  - id: mqtt-tls
    transport: mqtt
    options:
      session:
        broker_url: tls://mqtt.internal:8883
      credentials_uri: file://prod/mqtt/broker-creds

receivers:
  - id: ingest
    session_id: mqtt-tls
    topics:
      - topic: "sensors/#"
        qos: 1

senders:
  - id: forward
    session_id: mqtt-tls

bindings:
  - id: to-forward
    sender_id: forward
    address: processed/sensors

routes:
  - id: r1
    receiver_id: ingest
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-forward]
```

## Config Walkthrough

- `credentials_uri: file://prod/mqtt/broker-creds` resolves against the file
  store's base directory. The URI host+path becomes the on-disk path with a
  `.json` suffix: with a base directory of `/etc/gobridge/creds`, this file is
  `/etc/gobridge/creds/prod/mqtt/broker-creds.json`. The leading segment
  (`prod`) is part of the path, not stripped.
- The MQTT `broker_url` uses `tls://`; the resolved `TLSMaterial` (CA, client
  cert/key) is merged into transport options at build time and on every
  rotation. Term and field definitions live in
  [Credentials & HTTP API](../credentials-and-http-api.md).

### Credential file contents

The mounted file is the standard `file://` envelope. Only the `credentials`
object is required on read; `version`/`createdAt`/`updatedAt` are used by the
admin write path (optimistic locking) and are ignored when the bridge only reads
a mounted Secret.

```json
{
  "credentials": {
    "Password": { "Username": "mqtt-user", "Password": "s3cret" },
    "TLS": {
      "CertPEM": "-----BEGIN CERTIFICATE-----...",
      "KeyPEM": "-----BEGIN PRIVATE KEY-----...",
      "CAPEMs": ["-----BEGIN CERTIFICATE-----..."],
      "InsecureSkipVerify": false
    }
  },
  "version": 1
}
```

A file whose `credentials` field is missing or `null` is a hard
`INVALID_PAYLOAD` error, and an absent file is `NOT_FOUND` -- both permanent, so
the transport never connects anonymously.

## Go Bootstrap

The stock `gobridge` binary already registers the `file://` store from the
`-credentials-dir` flag, so no code is needed for the zero-code path (see
[Variation 1](#variation-1-stock-binary-flag)). A custom composition root wires
it explicitly and opts into rotation with `WithPolledCredentialStore`:

```go
package main

import (
    "context"
    "log/slog"
    "os"
    "time"

    "github.com/mariotoffia/gobridge/bridge"
    "github.com/mariotoffia/gobridge/config"
    "github.com/mariotoffia/gobridge/ports"
    goruntime "github.com/mariotoffia/gobridge/runtime"
    filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"
    "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto)

    fileRepo, err := filecreds.New("/etc/gobridge/creds")
    if err != nil {
        panic(err) // basePath empty, or not creatable AND not already mounted
    }
    resolver := goruntime.NewCredentialResolver()
    resolver.Register(fileRepo)

    rt, _ := bridge.NewBuilder(cfg,
        bridge.WithPolledCredentialStore(resolver, ports.PollBasedWrapperConfig{
            PollInterval: 5 * time.Minute,
            Jitter:       30 * time.Second, // ~10% de-sync across a fleet
            EmitOnStart:  true,             // surface a build-window rotation
        }),
    ).
        RegisterTransport("mqtt", paho.NewFactory(logger)). // config uses transport: mqtt
        Build(context.Background())

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    rt.Start(ctx)
    // ... wait for shutdown signal ...
    rt.Stop(ctx)
}
```

`filecreds.New` tolerates a read-only mount: if the base directory already
exists it is accepted, and the 0700 permission tighten is best-effort -- a chmod
that fails on a read-only or unowned mount (EROFS/EPERM) is logged at WARN, not
returned.

## Deep Dive: mounting the Secret

Store the JSON envelope as one Secret key, then project it to the path the URI
expects. `items[].path` may contain slashes, which builds the `prod/mqtt/`
subdirectories under the mount point.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: mqtt-broker-creds
type: Opaque
stringData:
  broker-creds.json: |
    {"credentials":{"Password":{"Username":"mqtt-user","Password":"s3cret"},
     "TLS":{"CertPEM":"...","KeyPEM":"...","CAPEMs":["..."],"InsecureSkipVerify":false}},
     "version":1}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gobridge
spec:
  replicas: 1
  selector: { matchLabels: { app: gobridge } }
  template:
    metadata: { labels: { app: gobridge } }
    spec:
      containers:
        - name: gobridge
          image: gobridge:latest
          args: ["-credentials-dir", "/etc/gobridge/creds"]
          volumeMounts:
            - name: creds
              mountPath: /etc/gobridge/creds
              readOnly: true
      volumes:
        - name: creds
          secret:
            secretName: mqtt-broker-creds
            defaultMode: 0400          # 0644 mounts trigger a one-time WARN
            items:
              - key: broker-creds.json
                path: prod/mqtt/broker-creds.json
```

Notes:

- **Read-only is the point.** `readOnly: true` plus `defaultMode: 0400` gives the
  file store an immutable, non-world-readable mount. A `defaultMode` of `0644`
  works but logs one WARN per file asking you to tighten it.
- **Do not use `subPath`.** A `subPath` mount does not receive Secret updates, so
  rotations would never reach the bridge. Mount the whole volume.

### Rotation and poll cadence

1. Update the `Secret` (kubectl, External Secrets, CSI). Kubelet refreshes the
   projected volume with an atomic `..data` symlink swap.
2. The credential poll wrapper re-resolves on its cadence -- `credential_poll_interval`
   in the AWS profile, or the `PollInterval` above (default 5 minutes). Reads are
   uncached, so a change is detected within one interval regardless of the
   resolver's TTL cache.
3. `CredentialSet.Equal` sees the difference; the refresher calls
   `ApplyCredentials` on each live session and increments
   `CredentialRotationApplied`.

Two propagation delays stack: kubelet's Secret sync period (tens of seconds by
default) and the bridge poll interval. Size `PollInterval` for your rotation SLA;
shrink it to reduce the window a hard rotation leaves sessions on revoked
credentials. For a hard rotation, a rejected apply (`NOT_AUTHORIZED`) triggers an
immediate reactive re-resolve rather than waiting for the next tick -- see
[Credential Rotation](../credentials-rotation.md#reactive-re-resolve-on-auth-failure).

## Crash Recovery / Failure Modes

- **Secret briefly unreadable (transient).** The resolver serves the
  last-known-good (expired) credential and increments `CredentialStaleServed`, so
  rebuilds keep working through a bounded blip. A local read error surfaces as
  `UNAVAILABLE`.
- **Secret key removed.** A missing file resolves to `NOT_FOUND` (permanent); it
  is not masked by stale-serving. Fix the mount.
- **Malformed JSON / empty `credentials`.** `INVALID_PAYLOAD` (permanent); the
  transport never connects anonymously.

## Variations

### Variation 1: Stock binary flag

No Go, no bootstrap JSON -- the image entrypoint reads the flag:

```yaml
args: ["-credentials-dir", "/etc/gobridge/creds"]
```

### Variation 2: AWS file-based deployment profile

Set the base directory and cadence through `BootstrapConfig` instead of a flag:

```json
{
  "credential_file_path": "/etc/gobridge/creds",
  "credential_poll_interval": "1m",
  "credential_emit_on_start": true
}
```

### Variation 3: Namespaced repository for multi-tenant layout

Register the store with a namespace so several schemes/prefixes can coexist on
one resolver:

```go
fileRepo, _ := filecreds.New("/etc/gobridge/creds", filecreds.WithNamespace("prod"))
```

## Related

- [Credentials & HTTP API](../credentials-and-http-api.md) -- URI schemes,
  resolver caching, and stale-while-error behavior.
- [Credential Rotation](../credentials-rotation.md) -- Pull/Push stores, the
  poll wrapper, and reactive re-resolve.
- [Scenario 15: HTTP Ingress with Credential-Based TLS Egress](15-http-ingress-with-credentials.md)
- [AWS Deployment Configuration](../aws-deployment/configuration.md#field-reference) -- the `credential_*` knobs.
