# GoBridge Admin API

Administrative HTTP API for managing GoBridge instances.

## Specification

- **File**: `admin-api.yaml`
- **OpenAPI Version**: 3.1.0
- **Default Port**: 8080

## Features

### Bridge Lifecycle
- `GET /bridge` - Get bridge status
- `POST /bridge/start` - Start the bridge
- `POST /bridge/stop` - Graceful shutdown
- `POST /bridge/drain` - Drain without stopping

### Connection Management
- `GET /connections` - List connections
- `POST /connections` - Create connection
- `GET /connections/{id}` - Get connection
- `PUT /connections/{id}` - Update connection
- `DELETE /connections/{id}` - Delete connection
- `POST /connections/{id}/reconnect` - Force reconnect
- `POST /connections/{id}/test` - Test connectivity

### Pipeline Management
- Full CRUD for pipelines
- Start/stop individual pipelines
- Get pipeline statistics

### DLQ Management
- View DLQ messages
- Replay messages to retry queue
- Purge DLQ
- Filter by topic, source, time range

### Configuration
- Get current configuration
- Reload from config source
- Validate before applying
- Preview changes (diff)

### Testing & Message Injection

The Admin API supports injecting test messages directly into pipelines:

```http
POST /api/v1/admin/test/message
Content-Type: application/json

{
  "pipelineId": "mqtt-to-sqs",
  "topic": "test/topic",
  "payload": "eyJoZWxsbyI6IndvcmxkIn0=",
  "waitForResult": true
}
```

This uses the **message-push middleware** (see below).

## Message-Push Middleware

The Admin API uses a special middleware for message injection:

```go
// middleware/inject/inject.go (to be implemented)

// InjectMiddleware allows messages to be injected into a pipeline
// bypassing the source. Used for testing and admin operations.
type InjectMiddleware struct {
    injectionCh chan *types.Message
}

func (m *InjectMiddleware) Inject(ctx context.Context, msg *types.Message) error {
    select {
    case m.injectionCh <- msg:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func (m *InjectMiddleware) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
    // Check for injected messages
    select {
    case injected := <-m.injectionCh:
        return next(ctx, injected)
    default:
        return next(ctx, msg)
    }
}
```

### Required Implementation

To support the testing endpoints, implement:

1. **InjectMiddleware** (`middleware/inject/`)
   - Injects messages into pipelines
   - Bypasses the source
   - Waits for processing result

2. **TestController** (`apis/http/admin/handlers/`)
   - Handles `/test/*` endpoints
   - Locates target pipeline
   - Injects message via middleware
   - Returns processing result

### Example Test Flow

```mermaid
sequenceDiagram
    participant Client
    participant AdminAPI
    participant InjectMW as InjectMiddleware
    participant Pipeline
    participant Target

    Client->>AdminAPI: POST /test/message
    AdminAPI->>InjectMW: Inject(message)
    InjectMW->>Pipeline: Process(message)
    Pipeline->>Target: Send(message)
    Target-->>Pipeline: result
    Pipeline-->>InjectMW: result
    InjectMW-->>AdminAPI: result
    AdminAPI-->>Client: TestMessageResult
```

## Authentication

All endpoints require authentication:

### Bearer Token (JWT)
```http
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...
```

### API Key
```http
X-API-Key: your-api-key-here
```

## Rate Limiting

Default: 100 requests per minute per API key

Configure via:
```yaml
admin:
  rateLimit:
    requestsPerMinute: 100
    burstSize: 20
```

## Error Handling

All errors return a standard format:

```json
{
  "code": "VALIDATION_ERROR",
  "message": "Invalid pipeline configuration",
  "details": {
    "field": "sourceConfig.topics",
    "reason": "at least one topic required"
  }
}
```

## CORS

Enable CORS for web clients:

```yaml
admin:
  cors:
    origins:
      - "https://dashboard.example.com"
    methods:
      - GET
      - POST
      - PUT
      - DELETE
    headers:
      - Authorization
      - Content-Type
```
