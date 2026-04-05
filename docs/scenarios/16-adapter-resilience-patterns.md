# Scenario 16: Adapter Resilience Patterns

Production resilience patterns implemented in GoBridge transport adapters.

## MQTT Session Resilience

### Properties Isolation
When multiple receivers share an MQTT session, each handler goroutine receives an independent deep-copy of the MQTT Properties (User properties, CorrelationData, etc.), preventing data races under concurrent dispatch.

### Case-Insensitive Error Classification
MQTT error messages are matched case-insensitively. "Connection Refused", "CONNECTION REFUSED", and "connection refused" are all correctly classified as `ErrConnectionLost`, enabling proper retry behavior regardless of broker error message formatting.

### Publish Timeout Safety Net
MQTT senders enforce a 60-second fallback timeout when no explicit timeout is configured, preventing indefinite hangs on stalled broker connections. When the caller's context already has a deadline, the fallback is not applied.

### Circuit Breaker Metrics
When the MQTT circuit breaker sender rejects a publish (circuit open), a `mqtt.publish.failures` metric with `reason=circuit_open` tag is emitted, providing visibility into fail-fast rejections.

## SQS Adapter Resilience

### Receiver Initialization Timeout
SQS receiver startup (client creation, queue URL resolution) is bounded by a 30-second timeout, preventing indefinite hangs when AWS credentials or endpoints are unavailable.

### Per-Poll Timeout
Each SQS ReceiveMessage call has an explicit timeout of `WaitTimeSeconds + 10` seconds, protecting against network-level stalls beyond the SQS long-poll window.

### Long-Poll Default
SQS receivers default to `WaitTimeSeconds=20` (long-polling) when not explicitly configured, preventing accidental short-polling which causes excessive API costs.

### Batch Error Classification
SQS batch send failures now distinguish between server faults (transient, retriable) and sender faults (permanent, not retriable). Messages with malformed payloads are classified as `ErrorRejected` and routed to DLQ instead of being retried indefinitely.

### Auto-Extend Ticker Adaptation
When `Extend()` changes the SQS visibility timeout, the auto-extend ticker interval updates accordingly, preventing excessive or insufficient extend calls.

### Processing Cancel at Construction
The `processingCancel` context function is set during delivery construction (before the auto-extend goroutine starts), eliminating a race window where extend failure could not cancel processing.

## HTTP Adapter Resilience

### Forward Error Classification
HTTP forwarder responses are classified by status code family:
- **5xx** -> `ErrUnavailable` (transient, retriable)
- **4xx** -> `ErrForwardFailed` (permanent, not retriable)

### Content-Type Validation
HTTP ingress validates `Content-Type: application/json` when the header is present, returning 415 Unsupported Media Type for non-JSON requests.

### Automatic Envelope ID
HTTP ingress generates a unique envelope ID when the request omits the `id` field, ensuring all messages have traceable identifiers.

## Configuration Example

```yaml
bridge:
  id: resilient-bridge

sessions:
  - id: mqtt-session
    transport: mqtt
    options:
      broker_urls: ["tcp://broker:1883"]
      client_id: resilient-bridge
      keep_alive: 30
      # Timeout applies to both publish and subscribe operations.
      # When 0, a 60s safety-net fallback is used.

receivers:
  - id: sqs-in
    transport: sqs
    options:
      queue_url: https://sqs.us-east-1.amazonaws.com/123/input
      # wait_time_seconds defaults to 20 (long-polling)
      # auto_extend defaults to true with adaptive ticker

senders:
  - id: mqtt-out
    transport: mqtt
    session: mqtt-session
    options:
      qos: 1
      # timeout defaults to 30s; 0 uses 60s safety-net

routes:
  - id: resilient-route
    receiver: sqs-in
    delivery_mode: direct_hold
    bindings: [mqtt-out]
    processors:
      - transform:
          mappings:
            - source: "$.data.value"
              target: "value"
      - circuitbreaker:
          failure_threshold: 5
          success_threshold: 2
          reset_timeout: 30s
```

## Resilience Behavior Summary

| Transport | Pattern | Default | Effect |
|-----------|---------|---------|--------|
| MQTT | Publish timeout fallback | 60s | Prevents indefinite hangs on stalled connections |
| MQTT | Properties deep-copy | Always | Prevents data races in multi-handler sessions |
| MQTT | Case-insensitive errors | Always | Correct error classification regardless of broker |
| MQTT | Circuit breaker metrics | Always | `mqtt.publish.failures` with `reason=circuit_open` |
| SQS | Receiver init timeout | 30s | Prevents startup hangs |
| SQS | Per-poll timeout | WaitTime+10s | Prevents network-level stalls |
| SQS | Long-poll default | 20s | Prevents accidental short-polling |
| SQS | Batch error classification | Always | SenderFault -> permanent, ServerFault -> transient |
| SQS | Adaptive auto-extend | Always | Ticker interval tracks visibility timeout changes |
| HTTP | Forward error classification | Always | 5xx transient, 4xx permanent |
| HTTP | Content-Type validation | Always | 415 for non-JSON when Content-Type present |
| HTTP | Auto envelope ID | Always | UUID generated when `id` field omitted |
