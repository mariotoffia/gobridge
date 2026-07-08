# Configuration System Overview

This guide covers the GoBridge configuration system for both **operators** writing YAML/JSON config files and **developers** using the programmatic Go API.

## Configuration Lifecycle

Every GoBridge instance follows this lifecycle from configuration to running bridge:

```mermaid
flowchart LR
    subgraph Sources
        F[File\nYAML / JSON]
        D[DynamoDB]
        C[Custom Loader]
    end

    F --> Parse
    D --> Parse
    C --> Parse

    Parse --> Merge
    Merge --> Validate
    Validate --> Build
    Build --> Runtime

    Runtime -. watch/reload .-> Parse

    style Runtime fill:#2d6,stroke:#333
```

1. **Load** -- A `Loader` reads raw configuration from a source (file, DynamoDB, or custom).
2. **Parse** -- YAML or JSON is unmarshalled into a `BridgeConfig` struct.
3. **Merge** -- When using layered configuration, overlays are merged onto the base.
4. **Validate** -- Structural validation checks required fields, referential integrity, and enum values.
5. **Build** -- The `Builder` (or `Supervisor`) creates a `Runtime` with live transports, sessions, and routes.
6. **Watch** *(optional)* -- A `Watcher` detects source changes and feeds them back into the lifecycle.

## Configuration Sources

### File Source (YAML / JSON)

The most common source. Reads a single file and auto-detects format by extension (`.yaml`, `.yml`, `.json`).

```go
import (
    fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
    "github.com/mariotoffia/gobridge/ports"
)

// NewSource requires a *ports.Registry carrying the transport/store decoders
// the config references; register them on the registry before loading.
reg := ports.NewRegistry()
source := fileconfig.NewSource("bridge.yaml", reg)
cfg, err := source.Load(ctx)
```

Maximum file size: **4 MiB**.

### DynamoDB Source

Stores configuration as a single DynamoDB item with version-based change detection. Useful for centralized config management in AWS environments.

```go
import ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"

loader := ddbconfig.NewLoader(ddbClient,
    ddbconfig.WithTableName("gobridge-config"),
    ddbconfig.WithBridgeID("production"),
    ddbconfig.WithPollInterval(30 * time.Second),
)
cfg, err := loader.Load(ctx)
```

Table schema: `PK = "config#<bridge-id>"`, `SK = "current"`, with a `version` field for change detection.

### Custom Sources

Implement `ports.Loader` and optionally `ports.Watcher`:

```go
// These are the canonical ports.Loader and ports.Watcher interfaces
// (ports/blueprint_loader.go); a custom source implements them directly.
type Loader interface {
    Load(ctx context.Context) (*ports.BridgeConfig, error)
}

type Watcher interface {
    Watch(ctx context.Context) (<-chan *ports.BridgeConfig, error)
}
```

## Layered Configuration

The `Manager` supports a base layer with ordered overlays. This enables patterns like base defaults + environment-specific overrides.

```mermaid
flowchart TD
    Base["Base Layer\n(file: defaults.yaml)"]
    Env["Overlay: Environment\n(DynamoDB: staging)"]
    Inst["Overlay: Instance\n(file: instance.yaml)"]

    Base --> M[DefaultMerge]
    Env --> M
    Inst --> M
    M --> V[Validate]
    V --> Merged["Merged BridgeConfig"]
```

**Merge rules** (`config.DefaultMerge`):
- `version`: overlay wins when non-zero; a zero overlay keeps the base version (it is an optimistic-concurrency counter, not reset to 0).
- `bridge` settings: overlay non-empty scalar fields win field-level; `bridge.cluster` is replaced wholesale when the overlay sets it.
- `config_watch`: overlay replaces base entirely if non-nil.
- `stores`: overlay replaces per-role (lease, outbox, dlq individually).
- `sessions`, `receivers`, `senders`, `bindings`: **merge by ID, field-level** -- new IDs are appended and a matching ID is merged field-by-field on top of the base entry. The base entry's typed plugin options (broker URLs, credentials) are **carried forward** unless the overlay changes the transport discriminator, so a partial patch (e.g. only `session_mode`) does not erase the plugin options the wire format drops (`json:"-"`).
- `routes`: **merge by ID, wholesale** -- a matching route is replaced entirely (routes carry no plugin options, so nothing can be lost); new IDs are appended.
- `http`: **merged field-level** (not wholesale) -- non-empty overlay scalars win, and the `admin_api_key` / `monitor_api_key` secrets are preserved when the overlay omits them or echoes back the `[REDACTED]` marker, so a partial patch cannot lock the operator out.

```go
mgr := config.NewManager(
    config.Layer{Name: "base", Loader: fileSource, Watcher: fileWatcher},
    config.WithOverlay(config.Layer{Name: "env", Loader: ddbLoader, Watcher: ddbLoader}),
    config.WithManagerLogger(logger),
)
cfg, err := mgr.Load(ctx)
```

## Two Configuration Paths

### 1. Declarative (YAML file)

Write a YAML file and let the framework do the rest. MQTT connection
options are nested under `session:` (and `sender:` for publish defaults).
Receivers, topics, and bindings inherit their transport from the referenced
session and carry no connection details of their own.

```yaml
bridge:
  id: my-bridge

sessions:
  - id: mqtt-conn
    transport: mqtt
    options:
      session:
        client_id: bridge-01
        broker_urls: ["tcp://localhost:1883"]

receivers:
  - id: sensor-in
    session_id: mqtt-conn
    topics:
      - topic: "sensors/#"
        qos: 1

senders:
  - id: sensor-out
    session_id: mqtt-conn
    options:
      sender:
        default_topic: archive/sensors
        qos: 1

bindings:
  - id: to-archive
    sender_id: sensor-out
    address: archive/sensors

routes:
  - id: forward
    receiver_id: sensor-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-archive]
```

### 2. Programmatic (Go Builder API)

Load config and wire everything in Go:

```go
cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto)

rt, err := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    RegisterTransport("mqtt", paho.NewFactory(logger)).
    RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
    RegisterProcessor("my-filter", filterProc).
    Build(ctx)

rt.Start(ctx)
defer rt.Stop(ctx)
```

## Minimal Working Example

The absolute smallest bridge config -- forwards MQTT messages between topics:

```yaml
bridge:
  id: minimal

sessions:
  - id: mqtt
    transport: mqtt
    options:
      session:
        client_id: minimal-bridge
        broker_urls: ["tcp://localhost:1883"]

receivers:
  - id: in
    session_id: mqtt
    topics:
      - topic: "source/#"

senders:
  - id: out
    session_id: mqtt
    options:
      sender:
        default_topic: destination/all

bindings:
  - id: fwd
    sender_id: out
    address: destination/all

routes:
  - id: route
    receiver_id: in
    bindings: [fwd]
```

## Go Bootstrap Pattern

The reference `cmd/gobridge/main.go` shows the wiring pattern. It is a minimal
demo/reference binary, **not** a production build (`cmd/gobridge/main.go:7`); a
production composition root registers only the transports and stores it actually
uses.

```go
func main() {
    // 1. Build the plugin registry, register the linked adapters, and load
    //    config. NewSource and NewWatcher both require the *ports.Registry.
    reg := ports.NewRegistry()
    _ = paho.Register(reg)         // mqtt transport decoder
    _ = nativestore.Register(reg)  // memory + sqlite store decoders
    fileSource := fileconfig.NewSource("bridge.yaml", reg)
    baseCfg, _ := fileSource.Load(ctx)

    // 2. Create file watcher using config_watch settings (same registry)
    fileWatcher := fileconfig.NewWatcher("bridge.yaml", reg,
        fileconfig.WithWatchConfig(baseCfg.ConfigWatch),
        fileconfig.WithLogger(logger),
    )

    // 3. Create manager (supports layered config)
    mgr := config.NewManager(
        config.Layer{Name: "file", Loader: fileSource, Watcher: fileWatcher},
        config.WithManagerLogger(logger),
    )
    cfg, _ := mgr.Load(ctx)

    // 4. Create supervisor with reconfiguration strategy
    sup := bridge.NewSupervisor(
        bridge.WithSupervisorLogger(logger),
        bridge.WithReconfigStrategy(
            bridge.NewWindowedStrategy(10*time.Second, 30*time.Second, nil),
        ),
    )

    // 5. Register transport and store factories
    sup.RegisterTransport("mqtt", paho.NewFactory(logger))
    sup.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())

    // 6. Start watching and run
    watchCh, _ := mgr.Watch(ctx)
    defer mgr.Stop()

    sup.Run(ctx, cfg, watchCh) // blocks until context cancelled
}
```

## Dynamic Reconfiguration

GoBridge supports live config reload without restarting the process.

### File Watcher Modes

Configured via the `config_watch` YAML section:

| Mode | Mechanism | Best For |
|------|-----------|----------|
| `notify` (default) | Filesystem events (fsnotify), debounced | Local disks, fast change detection |
| `poll` | Periodic SHA-256 content comparison | NFS, network mounts, containers |

```yaml
config_watch:
  mode: notify
  debounce: 200ms    # coalesce rapid writes
```

```yaml
config_watch:
  mode: poll
  poll_interval: 30s  # check every 30 seconds
```

### DynamoDB Watcher

Uses version-number polling. Only reloads the full item when the version changes.

### Supervisor Strategies

The `Supervisor` manages runtime lifecycle during reconfigurations:

| Strategy | Behaviour | Use Case |
|----------|-----------|----------|
| `DirectStrategy` | Apply every change immediately | Development, low-change environments |
| `DebouncedStrategy` | Wait for quiet period, emit latest | Burst edits (save-on-type) |
| `WindowedStrategy` | Debounce + hard deadline cap | Production (quiet=10s, max=30s) |

### Swap Modes

How the Supervisor transitions between old and new runtimes:

| Mode | Behaviour | When Used |
|------|-----------|-----------|
| `SwapOverlap` | Build new while old runs, then swap | Stateless transports (SQS) |
| `SwapPrepareCommit` | Validate first, stop old, then build new | Exclusive MQTT client IDs |
| `SwapAuto` (default) | Inspects `CapExclusiveIdentity`, picks mode | Recommended default |

## What's Next

| Document | Description |
|----------|-------------|
| [Configuration Stores](config-stores.md) | File, DynamoDB, and custom config backends; Manager layering; persistence |
| [Configuration Reference](configuration-reference.md) | Every config field documented |
| [Transport Configuration](transport-configuration.md) | MQTT, SQS, Azure SB, HTTP options |
| [Processors & Stores](processors-and-stores.md) | Filter, transform, circuit breaker, tenant; store backends |
| [Credentials & HTTP API](credentials-and-http-api.md) | Secret management and admin API |
| [Scenarios](scenarios/) | Progressive walkthroughs from simple to clustered |
