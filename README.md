# gobridge

A message bridge framework for connecting different transport technologies. Route messages between MQTT, SQS, Azure Service Bus, and more with middleware support for transformation, filtering, and retry handling.

## Features

- **Multi-transport support**: MQTT v5, AWS SQS, Azure Service Bus
- **Middleware chains**: Transform, filter, log, and retry messages
- **Pluggable architecture**: Easy to add new transports and middlewares
- **Clusterable**: External retry backing (e.g., SQS) for HA deployments
- **Observable**: Metrics and health check support

## Installation

gobridge uses a multi-module workspace to keep SDK dependencies separate.
The core module has **zero external dependencies** - only install what you need:

```bash
# Core module (types, interfaces, runtime) - no external deps
go get github.com/mariotoffia/gobridge

# MQTT transport (paho.golang)
go get github.com/mariotoffia/gobridge/transport/mqtt

# AWS transports (SQS)
go get github.com/mariotoffia/gobridge/transport/aws

# Azure transports (Service Bus)
go get github.com/mariotoffia/gobridge/transport/azure
```

## Quick Start

```go
package main

import (
    "context"
    "log"

    "github.com/mariotoffia/gobridge/bridge/core"
    "github.com/mariotoffia/gobridge/transport/mqtt"
    "github.com/mariotoffia/gobridge/transport/aws/sqs"
)

func main() {
    ctx := context.Background()

    // Create bridge
    bridge := core.NewBridge("my-bridge")

    // Register transport factories
    bridge.SourceRegistry().RegisterFactory(sqs.NewSourceFactory())
    bridge.TargetRegistry().RegisterFactory(mqtt.NewTargetFactory())

    // Create pipeline: SQS → MQTT
    pipeline, err := bridge.CreatePipeline(ctx, "sqs-to-mqtt",
        &sqs.SourceConfigImpl{
            ID: "sqs-source",
            Connection: sqs.ConnectionConfig{Region: "us-east-1"},
            QueueName: "my-queue",
        },
        &mqtt.TargetConfigImpl{
            ID: "mqtt-target",
            Connection: mqtt.ConnectionConfig{BrokerURL: "tcp://localhost:1883"},
            DefaultTopic: "events",
            QoS: 1, // At-least-once delivery
        },
        nil, // No middlewares
    )
    if err != nil {
        log.Fatal(err)
    }

    // Add and start
    bridge.AddPipeline(pipeline)
    if err := bridge.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Wait for shutdown signal...
    defer bridge.Close()
}
```

## Module Structure

```
gobridge/
├── go.work                 # Workspace definition
├── go.mod                  # Core module (zero external deps)
├── bridge/
│   ├── core/               # Pipeline, Route, Bridge runtime
│   └── types/              # Interfaces and common types
├── transport/
│   ├── mqtt/               # MQTT module (paho.golang)
│   │   └── go.mod
│   ├── aws/                # AWS module (AWS SDK v2)
│   │   ├── go.mod
│   │   └── sqs/            # SQS transport
│   └── azure/              # Azure module (Azure SDK)
│       ├── go.mod
│       └── servicebus/     # Service Bus transport
└── tests/
    └── docker/             # Docker test utilities
```

## Development

### Prerequisites

- Go 1.24 or later
- Docker (for integration tests)
- Make (optional, for convenience commands)

### Building

```bash
# Build all modules (using workspace)
make build

# Or manually
go build ./...
```

### Testing

```bash
# Run unit tests for all modules
make test

# Run integration tests (requires Docker)
make test-integration

# Run everything
make test-all
```

### Linting

```bash
# Install golangci-lint
make dev-deps

# Lint all modules
make lint
```

### Updating Dependencies

```bash
# Update all modules and sync workspace
make update

# Just tidy modules
make tidy
```

## Transports

### MQTT v5 (MQTT Module)

Uses [paho.golang](https://github.com/eclipse/paho.golang) for native MQTT v5 support.

```go
import "github.com/mariotoffia/gobridge/transport/mqtt"

source := &mqtt.SourceConfigImpl{
    ID: "mqtt-source",
    Connection: mqtt.ConnectionConfig{
        BrokerURL: "tcp://localhost:1883",
    },
    Topics: []string{"sensors/#"},
    QoS: 1,
}
```

### AWS SQS (AWS Module)

```go
import "github.com/mariotoffia/gobridge/transport/aws/sqs"

source := &sqs.SourceConfigImpl{
    ID: "sqs-source",
    Connection: sqs.ConnectionConfig{
        Region: "us-east-1",
        // For LocalStack:
        // Endpoint: "http://localhost:4566",
    },
    QueueName: "my-queue",
}
```

### Azure Service Bus (Azure Module)

```go
import "github.com/mariotoffia/gobridge/transport/azure/servicebus"

source := &servicebus.SourceConfigImpl{
    ID: "sb-source",
    Connection: servicebus.ConnectionConfig{
        ConnectionString: "Endpoint=sb://...",
    },
    QueueName: "my-queue",
}
```

## Delivery Guarantees

### MQTT QoS and External Retry

When using MQTT as a target with an external retry manager (e.g., SQS-backed):

- **Use QoS 1 or 2** - provides delivery confirmation via PUBACK/PUBCOMP
- When `Send()` returns `nil`, the broker has accepted the message
- Do NOT retry externally after successful send - the broker owns delivery

**Important**: QoS 0 should NOT be used with external retry managers because there's no confirmation that the broker received the message.

### SQS as Retry Backing Store

SQS provides durable, clusterable retry support:

1. Pipeline receives message from SQS
2. Sends to target (e.g., MQTT with QoS 1)
3. On success (PUBACK received): Ack SQS message
4. On failure: Nack SQS message for retry or send to DLQ

## Versioning

Each module is versioned independently:

- Core: `v1.x.x` (tags like `v1.0.0`)
- MQTT: `transport/mqtt/v1.x.x` (tags like `transport/mqtt/v1.0.0`)
- AWS: `transport/aws/v1.x.x` (tags like `transport/aws/v1.0.0`)
- Azure: `transport/azure/v1.x.x` (tags like `transport/azure/v1.0.0`)

## License

MIT License - see [LICENSE](LICENSE)
