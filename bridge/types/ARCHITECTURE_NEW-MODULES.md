# Proposed Module And Technology Layout

This document describes how the proposed architecture should fit a multi-module Go workspace.

The main rule is simple:

- keep ports and runtime contracts in the core module
- keep concrete technology implementations in their own modules

Related documents:

- [ARCHITECTURE_NEW.md](./ARCHITECTURE_NEW.md)
- [ARCHITECTURE_NEW-TRANSPORTS.md](./ARCHITECTURE_NEW-TRANSPORTS.md)
- [ARCHITECTURE_NEW-MIDDLEWARE.md](./ARCHITECTURE_NEW-MIDDLEWARE.md)
- [ARCHITECTURE_NEW-CLUSTERING.md](./ARCHITECTURE_NEW-CLUSTERING.md)
- [ARCHITECTURE_NEW-EXAMPLES.md](./ARCHITECTURE_NEW-EXAMPLES.md)
- [ARCHITECTURE_NEW-STORES.md](./ARCHITECTURE_NEW-STORES.md)
- [ARCHITECTURE_RECORDS.md](./ARCHITECTURE_RECORDS.md)

## Goals

- preserve the current multi-module pattern
- keep cloud SDK dependencies out of the bridge core
- make transport and storage adapters discoverable by technology family
- give lease store and durable outbox implementations a clear home

## Core Rule

The root module should own:

- `Envelope`
- `Delivery`
- `Receiver`
- `Sender`
- `Session`
- `Lease` interface
- route policy
- outbox and lease store interfaces
- runtime coordination logic

Technology modules should own:

- transport adapters
- lease store adapters
- durable outbox adapters
- config adapters
- credential adapters
- metrics exporters

## Recommended Top-Level Shape

Conceptually:

```text
gobridge/
  bridge/                      # core types and runtime
  technologies/                # technology-specific modules
    aws/
    azure/
    mqtt/
    native/
```

This is compatible with the current workspace model, but gives it a more consistent mirror structure.

## Mirror Tree Recommendation

Yes, a mirror tree is a good fit.

Recommended pattern:

```text
technologies/<family>/<capability>/<item>/
```

Examples:

- `technologies/aws/transport/sqs`
- `technologies/aws/transport/sns`
- `technologies/aws/store/dynamodblease`
- `technologies/aws/store/dynamodboutbox`
- `technologies/aws/config/dynamodb`
- `technologies/aws/credentials/pms`
- `technologies/aws/metrics/cloudwatch`
- `technologies/azure/transport/servicebus`
- `technologies/mqtt/transport/paho`
- `technologies/native/store/memorylease`
- `technologies/native/store/sqliteoutbox`
- `technologies/native/store/memoryoutbox`

This keeps all AWS-specific code together, all Azure-specific code together, and all provider-free local implementations together.

## Why `store` Is Better Than `repository`

For lease and outbox implementations, `store` is the better term.

Reason:

- these components are runtime persistence adapters
- they are not business-domain repositories
- they may expose conditional write, lease renewal, and replay operations that are more operational than domain-like

So the architecture should speak about:

- `LeaseStore`
- `OutboxStore`

not generic "repository" unless a specific module really is a repository abstraction.

## Concrete Port Placement

All port interfaces live in `bridge/types/`, not in `bridge/runtime/`. This ensures that technology adapter modules depend only on the contracts package, preserving correct dependency direction.

Core module:

```text
bridge/
  types/
    envelope.go
    delivery.go
    session.go
    route.go
    lease.go
    policy.go
    stores.go
    headers.go
    factories.go
  runtime/
    bridge.go
    route_runner.go
    session_manager.go
    outbox.go
    dlq.go
```

Technology modules:

```text
technologies/
  aws/
    store/
      dynamodblease/
      dynamodboutbox/
    transport/
      sqs/
      sns/
  azure/
    transport/
      servicebus/
  mqtt/
    transport/
      paho/
  native/
    store/
      memorylease/
      sqliteoutbox/
      memoryoutbox/
```

Each concrete adapter module can have its own `go.mod`.

## What Goes Into A Technology Module

An adapter module should contain:

- the concrete implementation
- adapter-specific config types
- provider SDK dependencies
- tests for that adapter

It should not redefine the bridge contracts.

Those remain in the core module.

## Shared Lease Store And Shared Durable Outbox

Yes, these belong in technology modules as storage adapters.

Examples:

- `technologies/aws/store/dynamodblease`
- `technologies/aws/store/dynamodboutbox`
- `technologies/native/store/sqliteoutbox`
- `technologies/native/store/memorylease`

Interpretation:

- `dynamodblease` is a clustered `LeaseStore` implementation
- `dynamodboutbox` is a clustered `OutboxStore` implementation
- `sqliteoutbox` is a local durable outbox for standalone or simple deployments
- `memorylease` is only suitable for tests or single-process mode

## Store Backend Strategy

### DynamoDB First

DynamoDB is the phase-1 production store for both `LeaseStore` and `OutboxStore`. Rationale:

- Single-digit-millisecond latency for lease renewal hot paths
- Conditional writes map directly to fencing semantics
- TTL-based automatic item expiration aligns with outbox compaction
- On-demand capacity mode handles bursty bridge workloads
- Multi-AZ durability by default
- Matches AWS-first platform direction

See [ARCHITECTURE_NEW-STORES.md](./ARCHITECTURE_NEW-STORES.md) for table schemas and operational guidance.

### SQLite And Memory For Local/Dev

`sqliteoutbox` and `memorylease` are native test adapters:

- `memorylease`: In-memory lease store for unit tests and single-process mode
- `sqliteoutbox`: File-based durable outbox for integration tests and single-process deployments
- `memoryoutbox`: In-memory outbox for unit tests where durability is not under test

These are not suitable for clustered production deployments.

### Postgres Later

Production SQL support is not phase-1 scope. If added later:

- Choose Postgres explicitly, not generic SQL
- Avoid abstracting over SQL dialects prematurely
- DynamoDB remains the primary cloud deployment target
- The store interfaces in `bridge/types/` are already backend-neutral

## Why This Helps The Clustered Design

The clustered architecture needs shared infrastructure implementations, but those implementations should not pull cloud SDKs into the root module.

With this layout:

- the core runtime depends only on interfaces
- the runtime can be wired with any supported `LeaseStore`
- the runtime can be wired with any supported `OutboxStore`
- cluster patterns remain generic even though the backing technology changes

## Mapping From The Current Workspace

The current repo already trends in this direction:

- `transport/aws`
- `transport/azure`
- `transport/mqtt`
- `config/aws/dynamodb`
- `credentials/aws/pms`
- `metrics/aws/cloudwatch`

The proposed mirror tree simply makes that pattern consistent across:

- transport
- config
- credentials
- metrics
- lease store
- durable outbox

## Migration Strategy

This does not require a big-bang refactor.

Recommended sequence:

1. keep the core module as the source of ports and runtime contracts
2. define `LeaseStore` and `OutboxStore` interfaces in `bridge/types/`
3. add new technology modules under `technologies/...`
4. migrate existing transport and config modules incrementally
5. keep `go.work` as the workspace aggregation layer

## Example Wiring

Conceptually:

```go
runtime := bridge.NewRuntime(
    bridge.WithLeaseStore(dynamodblease.NewStore(...)),
    bridge.WithOutboxStore(dynamodboutbox.NewStore(...)),
    bridge.WithSenderFactory(mqttpaho.NewFactory(...)),
    bridge.WithReceiverFactory(awssqs.NewFactory(...)),
)
```

The runtime depends only on the interfaces.

The technology choice is made by composition.

## Recommendation

Yes, use the mirror-tree idea.

But make the split explicit:

- core runtime contracts stay in the root module
- concrete transport and storage adapters live in technology modules
- shared lease store and shared durable outbox should be modeled as `store` adapters, not as core implementations
