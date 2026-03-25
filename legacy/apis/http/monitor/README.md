# GoBridge Monitor API

Observability and monitoring HTTP API for GoBridge instances and clusters.

## Specification

- **File**: `monitor-api.yaml`
- **OpenAPI Version**: 3.1.0
- **Default Port**: 8081

## Features

### Health Checks (Kubernetes-compatible)

| Endpoint | Purpose | Use Case |
|----------|---------|----------|
| `GET /health` | Full health status | Dashboard |
| `GET /health/live` | Liveness probe | `livenessProbe` |
| `GET /health/ready` | Readiness probe | `readinessProbe` |
| `GET /health/startup` | Startup probe | `startupProbe` |

Example Kubernetes deployment:

```yaml
livenessProbe:
  httpGet:
    path: /api/v1/monitor/health/live
    port: 8081
  initialDelaySeconds: 10
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /api/v1/monitor/health/ready
    port: 8081
  initialDelaySeconds: 5
  periodSeconds: 5

startupProbe:
  httpGet:
    path: /api/v1/monitor/health/startup
    port: 8081
  failureThreshold: 30
  periodSeconds: 10
```

### Metrics (Prometheus-compatible)

```http
GET /api/v1/monitor/metrics
Accept: text/plain
```

Returns:
```prometheus
# HELP gobridge_messages_received_total Total messages received
# TYPE gobridge_messages_received_total counter
gobridge_messages_received_total{pipeline="mqtt-to-sqs"} 12345

# HELP gobridge_messages_sent_total Total messages sent  
# TYPE gobridge_messages_sent_total counter
gobridge_messages_sent_total{pipeline="mqtt-to-sqs"} 12340

# HELP gobridge_message_latency_seconds Message processing latency
# TYPE gobridge_message_latency_seconds histogram
gobridge_message_latency_seconds_bucket{pipeline="mqtt-to-sqs",le="0.01"} 10000
gobridge_message_latency_seconds_bucket{pipeline="mqtt-to-sqs",le="0.1"} 12000
gobridge_message_latency_seconds_bucket{pipeline="mqtt-to-sqs",le="+Inf"} 12345

# HELP gobridge_in_flight Current in-flight messages
# TYPE gobridge_in_flight gauge
gobridge_in_flight{pipeline="mqtt-to-sqs"} 15
```

Prometheus scrape config:
```yaml
scrape_configs:
  - job_name: 'gobridge'
    static_configs:
      - targets: ['localhost:8081']
    metrics_path: /api/v1/monitor/metrics
```

### OpenTelemetry Tracing

The Monitor API provides full OpenTelemetry integration.

#### Trace Context Propagation

All requests can include W3C TraceContext headers:

```http
GET /api/v1/monitor/traces
traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01
tracestate: congo=t61rcWkgMzE
```

#### View Traces

```http
GET /api/v1/monitor/traces?limit=100&minDuration=100ms
```

Response:
```json
{
  "traces": [
    {
      "traceId": "0af7651916cd43dd8448eb211c80319c",
      "rootSpan": "pipeline.process",
      "operationName": "mqtt-to-sqs.process",
      "startTime": "2024-01-15T10:30:00Z",
      "duration": "15ms",
      "spanCount": 5,
      "status": "ok"
    }
  ],
  "total": 1543,
  "hasMore": true
}
```

#### Trace Details

```http
GET /api/v1/monitor/traces/0af7651916cd43dd8448eb211c80319c
```

Returns full trace with all spans, attributes, and events.

#### Configure Tracing

```http
PUT /api/v1/monitor/traces/config
Content-Type: application/json

{
  "enabled": true,
  "samplingRate": 0.1,
  "exporters": [
    {
      "type": "otlp",
      "endpoint": "localhost:4317"
    },
    {
      "type": "jaeger",
      "endpoint": "http://jaeger:14268/api/traces"
    }
  ]
}
```

### Instance Monitoring

Monitor individual bridge instances:

```http
GET /api/v1/monitor/instances
```

Response:
```json
[
  {
    "id": "bridge-1",
    "clusterId": "prod-cluster",
    "hostname": "bridge-pod-abc123",
    "status": "healthy",
    "version": "1.2.0",
    "isLeader": true,
    "pipelinesCount": 5,
    "connectionsCount": 2
  }
]
```

### Cluster Monitoring

Get cluster-wide status:

```http
GET /api/v1/monitor/cluster
```

Response:
```json
{
  "clusterId": "prod-cluster",
  "status": "healthy",
  "instanceCount": 3,
  "healthyInstances": 3,
  "leaderElected": true,
  "leaderId": "bridge-1"
}
```

### Real-time Streaming (WebSocket)

#### Metrics Stream

```javascript
const ws = new WebSocket('ws://localhost:8081/api/v1/monitor/stream/metrics');
ws.onmessage = (event) => {
  const data = JSON.parse(event.data);
  console.log('Metrics update:', data);
};
```

#### Logs Stream

```javascript
const ws = new WebSocket('ws://localhost:8081/api/v1/monitor/stream/logs?level=error');
ws.onmessage = (event) => {
  const log = JSON.parse(event.data);
  console.log(`[${log.level}] ${log.message}`);
};
```

#### Traces Stream

```javascript
const ws = new WebSocket('ws://localhost:8081/api/v1/monitor/stream/traces');
ws.onmessage = (event) => {
  const trace = JSON.parse(event.data);
  console.log('New trace:', trace.traceId);
};
```

### Alerting

#### List Active Alerts

```http
GET /api/v1/monitor/alerts?severity=critical
```

#### Create Alert Rule

```http
POST /api/v1/monitor/alerts/rules
Content-Type: application/json

{
  "name": "high-error-rate",
  "expression": "rate(gobridge_messages_failed_total[5m]) > 0.1",
  "severity": "critical",
  "duration": "2m",
  "annotations": {
    "summary": "Pipeline error rate above 10%"
  }
}
```

## OpenTelemetry Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         GoBridge                                 │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                    Monitor API                           │    │
│  │  ┌─────────────┐ ┌─────────────┐ ┌─────────────────┐    │    │
│  │  │   /traces   │ │  /metrics   │ │ /stream/*       │    │    │
│  │  └──────┬──────┘ └──────┬──────┘ └────────┬────────┘    │    │
│  │         │               │                  │             │    │
│  └─────────┼───────────────┼──────────────────┼─────────────┘    │
│            │               │                  │                  │
│  ┌─────────▼───────────────▼──────────────────▼─────────────┐    │
│  │              OpenTelemetry SDK                            │    │
│  │  ┌─────────────┐ ┌─────────────┐ ┌─────────────────┐     │    │
│  │  │TracerProvider│ │MeterProvider│ │  LoggerProvider │     │    │
│  │  └──────┬──────┘ └──────┬──────┘ └────────┬────────┘     │    │
│  └─────────┼───────────────┼──────────────────┼─────────────┘    │
└────────────┼───────────────┼──────────────────┼──────────────────┘
             │               │                  │
             ▼               ▼                  ▼
    ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐
    │   Jaeger    │  │ Prometheus  │  │     Loki        │
    │   Zipkin    │  │    OTLP     │  │     OTLP        │
    │   OTLP      │  │             │  │                 │
    └─────────────┘  └─────────────┘  └─────────────────┘
```

## Span Naming Convention

GoBridge uses consistent span naming:

| Operation | Span Name | Kind |
|-----------|-----------|------|
| Message receive | `{pipeline}.receive` | CONSUMER |
| Middleware process | `{pipeline}.middleware.{name}` | INTERNAL |
| Target send | `{pipeline}.send` | PRODUCER |
| Connection | `connection.{id}.publish` | CLIENT |
| Retry | `retry.transport` / `retry.message` | INTERNAL |

## Metrics Naming Convention

All metrics use the `gobridge_` prefix:

| Metric | Type | Labels |
|--------|------|--------|
| `gobridge_messages_received_total` | Counter | pipeline |
| `gobridge_messages_sent_total` | Counter | pipeline |
| `gobridge_messages_failed_total` | Counter | pipeline, error_code |
| `gobridge_message_latency_seconds` | Histogram | pipeline |
| `gobridge_in_flight` | Gauge | pipeline |
| `gobridge_retry_attempts_total` | Counter | pipeline, type |
| `gobridge_connection_reconnects_total` | Counter | connection |
| `gobridge_dlq_messages` | Gauge | - |

## Security

The Monitor API is designed for internal use:

- Deploy on internal network only
- Use network policies in Kubernetes
- Consider authentication for sensitive endpoints
- Always use HTTPS in production

## Configuration

```yaml
monitor:
  port: 8081
  enableAuth: false  # Internal only
  cors:
    enabled: true
    origins: ["*"]
  
  tracing:
    enabled: true
    samplingRate: 0.1
    exporters:
      - type: otlp
        endpoint: otel-collector:4317
  
  metrics:
    enabled: true
    path: /api/v1/monitor/metrics
  
  streaming:
    enabled: true
    maxConnections: 100
```
