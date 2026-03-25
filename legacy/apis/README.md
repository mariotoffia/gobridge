# GoBridge HTTP APIs

This directory contains OpenAPI 3.1 specifications for GoBridge HTTP APIs.

## Overview

GoBridge provides two separate APIs:

| API | Purpose | Default Port | Authentication |
|-----|---------|--------------|----------------|
| **Admin API** | Bridge administration and management | 8080 | Bearer/API Key |
| **Monitor API** | Observability and monitoring | 8081 | Optional |

## Implementation Status

| Component | Location | Status |
|-----------|----------|--------|
| Admin API OpenAPI Spec | `http/admin/admin-api.yaml` | ✅ Complete |
| Admin API Handlers | `http/server/admin_handlers.go` | ✅ Complete |
| Monitor API OpenAPI Spec | `http/monitor/monitor-api.yaml` | ✅ Complete |
| Monitor API Handlers | `http/server/monitor_handlers.go` | ✅ Complete |
| WebSocket Streaming | `http/server/websocket.go` | ✅ Complete |
| InjectMiddleware | `../../middleware/inject/inject.go` | ✅ Complete |
| DLQ Management Integration | `../../middleware/retry/dlq_manager.go` | ✅ Complete |
| Config Reload Integration | `../../bridge/core/config_reload.go` | ✅ Complete |

### Feature Integration

The HTTP API handlers are integrated with the following subsystems:

| Feature | Handler | Backend Implementation |
|---------|---------|------------------------|
| DLQ Summary | `GET /dlq` | `types.DLQManager.GetSummary()` |
| DLQ Messages | `GET /dlq/messages` | `types.DLQManager.ListMessages()` |
| DLQ Replay | `POST /dlq/replay` | `types.DLQManager.Replay()` |
| DLQ Purge | `POST /dlq/purge` | `types.DLQManager.Purge()` |
| Config Reload | `POST /config/reload` | `ConfigReloaderInterface.Reload()` |

## API Specifications

### Admin API (`http/admin/admin-api.yaml`)

The Admin API provides endpoints for:

- **Bridge Lifecycle**: Start, stop, drain operations
- **Connections**: CRUD operations for transport connections (MQTT, SQS, etc.)
- **Pipelines**: Create, update, delete message pipelines
- **Routes**: Chain multiple pipelines together
- **Subscriptions**: Manage topic subscriptions
- **Publishers**: Manage message publishers
- **DLQ Management**: Dead Letter Queue operations (view, replay, purge)
- **Configuration**: Reload, validate, and diff configurations
- **Testing**: Inject test messages via pipelines
- **Diagnostics**: Logs, errors, debug information

### Monitor API (`http/monitor/monitor-api.yaml`)

The Monitor API provides endpoints for:

- **Health Checks**: Kubernetes-compatible liveness/readiness probes
- **Metrics**: Prometheus-compatible metrics export
- **Tracing**: OpenTelemetry-compatible distributed tracing
- **Instances**: Monitor individual bridge instances
- **Cluster**: Cluster-wide monitoring and topology
- **Alerts**: Alert management and rules
- **Streaming**: WebSocket endpoints for real-time data

## Quick Start

### Viewing the Specifications

You can view these OpenAPI specifications using:

1. **Swagger UI**: Use the [Swagger Editor](https://editor.swagger.io/) online
2. **VS Code**: Install the "OpenAPI (Swagger) Editor" extension
3. **Redoc**: Generate beautiful documentation with [Redoc](https://github.com/Redocly/redoc)

### Generating Code

Generate client/server code using [OpenAPI Generator](https://openapi-generator.tech/):

```bash
# Generate Go server
openapi-generator generate -i apis/http/admin/admin-api.yaml -g go-server -o apis/http/admin/server

# Generate Go client
openapi-generator generate -i apis/http/admin/admin-api.yaml -g go -o apis/http/admin/client

# Generate TypeScript client
openapi-generator generate -i apis/http/monitor/monitor-api.yaml -g typescript-axios -o apis/http/monitor/client-ts
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Load Balancer                             │
└─────────────────────────────────────────────────────────────────┘
                    │                           │
                    ▼                           ▼
          ┌─────────────────┐         ┌─────────────────┐
          │   Admin API     │         │   Monitor API   │
          │   :8080         │         │   :8081         │
          └────────┬────────┘         └────────┬────────┘
                   │                           │
                   └───────────┬───────────────┘
                               │
                               ▼
          ┌─────────────────────────────────────────┐
          │              GoBridge Core              │
          │  ┌─────────┐  ┌─────────┐  ┌─────────┐  │
          │  │Pipeline1│  │Pipeline2│  │PipelineN│  │
          │  └─────────┘  └─────────┘  └─────────┘  │
          │                                         │
          │  ┌───────────┐  ┌────────────────────┐  │
          │  │Connections│  │OpenTelemetry Export│  │
          │  └───────────┘  └────────────────────┘  │
          └─────────────────────────────────────────┘
```

## OpenTelemetry Integration

The Monitor API integrates with OpenTelemetry for:

### Tracing
- W3C TraceContext propagation
- Span export to Jaeger, Zipkin, or OTLP backends
- Real-time trace viewing and search

### Metrics
- Prometheus-compatible `/metrics` endpoint
- OTLP metric export
- Custom metrics for pipelines, connections, retry

### Logging
- Trace ID correlation in logs
- Structured JSON logging
- Real-time log streaming via WebSocket

## Security

### Admin API

The Admin API requires authentication:

```yaml
# Bearer Token
Authorization: Bearer <jwt-token>

# API Key
X-API-Key: <api-key>
```

### Monitor API

The Monitor API is designed for internal monitoring and can optionally require authentication.

For production:
- Use network policies to restrict access
- Consider authentication for sensitive endpoints
- Enable HTTPS/TLS

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ADMIN_API_PORT` | 8080 | Admin API port |
| `MONITOR_API_PORT` | 8081 | Monitor API port |
| `ADMIN_API_KEY` | - | API key for admin access |
| `ENABLE_AUTH` | true | Enable authentication |
| `CORS_ORIGINS` | * | CORS allowed origins |

## Related Documentation

- [ARCHITECTURE.md](../bridge/types/ARCHITECTURE.md) - Overall architecture
- [ARCHITECTURE-MIDDLEWARE.md](../bridge/types/ARCHITECTURE-MIDDLEWARE.md) - Middleware and retry
- [MISSING.md](../bridge/types/MISSING.md) - Implementation status
