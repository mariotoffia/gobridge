# Long-Running Integration Tests

Stress tests for gobridge exercising realistic multi-transport, multi-instance
scenarios under sustained load. **All messages must be accounted for -- zero
loss tolerance.**

## Running

```bash
# Requires Docker for ElasticMQ, Mosquitto, DynamoDB Local
make test-long-running

# Single test
go test -race -timeout 1200s -v -tags=longrunning -run TestUC1 ./tests/longrunning/...

# With environment overrides (skip container auto-start)
DYNAMODB_ENDPOINT=http://127.0.0.1:8000 \
SQS_ENDPOINT=http://127.0.0.1:9324 \
MQTT_BROKER_URL=tcp://127.0.0.1:1883 \
  go test -race -timeout 1200s -v -tags=longrunning ./tests/longrunning/...
```

## Mosquitto Configuration

TestMain configures Mosquitto for high throughput:

```
persistence          true
max_inflight_messages 0    # unlimited
max_queued_messages   0    # unlimited
max_queued_bytes      0    # unlimited
```

This prevents the broker from dropping messages under load.

## File Overview

```
tests/longrunning/
  longrunning_test.go            # TestMain + shared helpers (SQS/MQTT/DDB setup)
  longrunning_helpers_test.go    # Reusable processors and wrapper senders
  uc1_sqs_mqtt_sqs_test.go       # UC1:  Clustered SQS -> MQTT -> SQS
  uc2_mqtt_fanout_sqs_test.go    # UC2:  Content-routed fan-out
  uc3_cluster_failover_test.go   # UC3:  3-instance cascading failover
  uc4_bidirectional_test.go      # UC4:  Bidirectional SQS <-> MQTT
  uc5_pipeline_chain_test.go     # UC5:  4-stage pipeline chain
  uc6_burst_backpressure_test.go # UC6:  Burst + poison -> DLQ
  uc7_transport_combos_test.go   # UC7-11: Transport combinations
  uc12_cluster_lease_test.go     # UC12-16: Cluster & lease coordination
  uc17_message_shape_test.go     # UC17-21: Message size & shape
  uc22_routing_filtering_test.go # UC22-26: Routing & filtering
  uc27_failure_recovery_test.go  # UC27-29: Failure & recovery (part A)
  uc27_failure_recovery_b_test.go# UC30-32: Failure & recovery (part B)
  uc33_backpressure_test.go      # UC33-37: Backpressure & concurrency
  uc38_outbox_modes_test.go      # UC38-41: Outbox delivery modes
```

---

## A. Transport Combinations (UC1, UC7-UC11)

### UC1: SQS -> MQTT ($shared) -> SQS -- Clustered Load Balancing

```
SQS-IN --> [Bridge-A] --+
           [Bridge-B] --+--> MQTT "uc1/pipeline/data" (QoS 1)
           [Bridge-C] --+         |
                        +---------+
                        |
                [Bridge-D] --> SQS-OUT-1
                [Bridge-E] --> SQS-OUT-2
```

| Parameter | Value |
|-----------|-------|
| Volume | 5,000 messages |
| Timeout | 120s |
| Delivery (ingress) | SharedOutbox + exclusive session |
| Delivery (egress) | DirectHold |
| Session | Exclusive (A,B,C), Ephemeral (D,E) |

**Assertions:** Each output queue gets 5,000 (fan-out). No duplicates. DLQ empty.

### UC7: SQS FIFO Ordering Through MQTT

```
SQS-IN (3 groups: G1, G2, G3)
  --> [Bridge] --> MQTT "uc7/ordered"
    --> [Egress] --> SQS-OUT
```

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (1,000 per group) |
| Timeout | 120s |
| Delivery | DirectHold |

**Assertions:** 3,000 messages in SQS-OUT. Soft per-group ordering via headers.

### UC8: Multi-Protocol Fan-Out

```
SQS-IN --> [Bridge (DispatchFanOut + MatchAll)]
  --> MQTT "uc8/alpha"
  --> MQTT "uc8/beta"
  --> SQS-OUT
```

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 |
| Timeout | 120s |
| Dispatch | FanOut |
| Delivery | SharedOutbox |

**Assertions:** All 3 targets get 2,000. DLQ empty.

### UC9: MQTT QoS 2 Stress

```
MQTT Publisher (QoS 2) --> "uc9/input"
  --> [Bridge (QoS 2)] --> "uc9/output" --> Collector
```

| Parameter | Value |
|-----------|-------|
| Volume | 5,000 |
| Timeout | 120s |
| QoS | 2 (both inbound/outbound) |

**Assertions:** 5,000 unique messages (exactly-once). Zero duplicates.

### UC10: HTTP Inject to MQTT

```
runtime.Inject(routeID, env)
  --> [Bridge + stageProcessor] --> MQTT "uc10/output" --> Collector
```

| Parameter | Value |
|-----------|-------|
| Volume | 1,000 (via Inject API) |
| Timeout | 60s |
| Source | noopReceiver (blocks until cancel) |

**Assertions:** 1,000 messages with `stage_inject` header. DLQ empty.

### UC11: SQS to SQS Direct (No MQTT)

```
SQS-IN --> [Bridge (SharedOutbox)] --> SQS-OUT
```

| Parameter | Value |
|-----------|-------|
| Volume | 5,000 |
| Timeout | 120s |
| Delivery | SharedOutbox |

**Assertions:** 5,000 in SQS-OUT. No duplicates. No MQTT involvement.

---

## B. Cluster & Lease Coordination (UC3, UC12-UC16)

### UC3: 3-Instance Cascading Failover

```
SQS-IN --> [A] --+
          [B] --+--> MQTT "uc3/output/data" --> Collector
          [C] --+

Timeline:
  T+0: All start. One acquires lease.
  T+N: Stop leader --> second takes over
  T+M: Stop second leader --> third finishes
```

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 |
| Timeout | 180s |
| Delivery | SharedOutbox |
| Session | Exclusive (shared lease) |

**Assertions:** 2,000 unique payloads. DLQ empty.

### UC12: Rolling Restart (Zero Downtime)

```
SQS-IN --> [A, B, C] --> MQTT "uc12/output" --> Collector

Sequence: Stop A, create A', start A'. Stop B, create B', start B'.
```

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 |
| Timeout | 180s |
| Delivery | SharedOutbox |

**Assertions:** >= 3,000 unique payloads. DLQ empty.

### UC13: Split-Brain Recovery

```
SQS-IN --> [A (crashes)] --> [B (acquires expired lease)]
  --> MQTT "uc13/output" --> Collector
```

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 |
| Timeout | 180s |
| Failure mode | Context cancel (no graceful Stop) |

**Assertions:** >= 2,000 unique payloads. Fencing tokens prevent stale sends.

### UC14: Lease Contention -- 10 Instances

```
SQS-IN --> [I0..I9] --> MQTT "uc14/output" --> Collector
```

| Parameter | Value |
|-----------|-------|
| Volume | 1,000 |
| Timeout | 180s |
| Instances | 10 competing |

**Assertions:** Exactly 1 "active" at each sample point (5 samples). >= 1,000 delivered.

### UC15: ConnectAfterLease

```
SQS-IN --> [A (active, ConnectAfterLease=true)]
           [B (standby, ConnectAfterLease=true)]
  --> MQTT "uc15/output" --> Collector
```

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 |
| Timeout | 180s |
| ConnectAfterLease | true |

**Assertions:** Standby does not connect until lease acquired. >= 2,000 delivered.

### UC16: Multi-Session Cluster (Two Independent Leases)

```
SQS-IN-1 --> [Route-Alpha (session-alpha)] --> MQTT "uc16/alpha"
SQS-IN-2 --> [Route-Beta  (session-beta)]  --> MQTT "uc16/beta"
```

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 (1,000 per route) |
| Timeout | 120s |
| Sessions | 2 independent exclusive sessions |

**Assertions:** Alpha gets 1,000, Beta gets 1,000. No cross-contamination.

---

## C. Message Size & Shape (UC17-UC21)

### UC17: Large Payloads (200KB)

| Parameter | Value |
|-----------|-------|
| Volume | 500 x 200KB |
| Timeout | 180s |
| Topology | SQS -> MQTT -> SQS round-trip |
| Integrity | SHA256 hash verification |

### UC18: Tiny Payloads High Throughput

| Parameter | Value |
|-----------|-------|
| Volume | 50,000 x 10B |
| Timeout | 300s |
| Topology | SQS -> MQTT -> Collector |
| Verify | All seq 0-49999 present |

### UC19: Mixed Payload Sizes

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (1K tiny 50B + 1K medium 10KB + 1K large 100KB) |
| Timeout | 180s |
| Verify | Count per size_class header |

### UC20: Header-Heavy (50 Headers)

| Parameter | Value |
|-----------|-------|
| Volume | 1,000 x 50 headers |
| Timeout | 120s |
| Note | SQS limits to 10 attrs; rest via MQTT user properties |

### UC21: Binary Payload Round-Trip

| Parameter | Value |
|-----------|-------|
| Volume | 1,000 x random bytes (incl. 0x00, 0xFF) |
| Timeout | 120s |
| Integrity | SHA256 hash verification after base64 round-trip |

---

## D. Routing & Filtering (UC2, UC22-UC26)

### UC2: MQTT -> Content-Routed Fan-Out to 3 SQS Queues

```
MQTT "uc2/devices/+/telemetry" --> [Bridge (MatchByHeader)]
  --> SQS-A (factory=A)
  --> SQS-B (factory=B)
  --> SQS-C (factory=C)
```

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (1,000 per factory) |
| Timeout | 90s |
| Routing | MatchByHeader("factory", ...) |

### UC22: 10-Rule MatchRule Routing

```
SQS-IN --> [Bridge (10 compiled MatchRules)] --> SQS-Q0..Q9
```

| Parameter | Value |
|-----------|-------|
| Volume | 5,000 (500 per rule) |
| Timeout | 120s |
| Operators | eq, prefix, contains, regex, gt, in |

### UC23: Subject Prefix Routing

```
MQTT "uc23/#" --> [Bridge (MatchBySubjectPrefix)]
  --> SQS-orders  (prefix "uc23/orders/")
  --> SQS-events  (prefix "uc23/events/")
  --> SQS-metrics (prefix "uc23/metrics/")
```

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 |
| Timeout | 90s |

### UC24: Dynamic Address Templates

```
SQS-IN --> [Bridge (address="uc24/{tenant}/{region}/data")]
  --> MQTT uc24/acme/us/data
  --> MQTT uc24/globex/eu/data
  --> MQTT uc24/initech/ap/data
```

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (1,000 per combo) |
| Timeout | 90s |

### UC25: Filter Processor (90% Drop)

```
SQS-IN (10,000) --> [Bridge + filterProcessor(keep seq%10==0)]
  --> MQTT (1,000 passed)
  --> DLQ  (9,000 filtered)
```

| Parameter | Value |
|-----------|-------|
| Volume | 10,000 |
| Timeout | 180s |
| OnPermanentFailure | FailureDLQ |

**Assertions:** 1,000 + 9,000 = 10,000. Full accounting.

### UC26: 5-Stage Processor Chain

```
SQS-IN --> [Bridge + p1,p2,p3,p4,p5] --> MQTT --> [Bridge-B] --> SQS-OUT
```

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 |
| Timeout | 120s |
| Verify | chain_order="p1,p2,p3,p4,p5" |

---

## E. Failure & Recovery (UC6, UC27-UC32)

### UC6: Burst Backpressure + Poison DLQ

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (2,500 normal + 500 poison) |
| MaxInFlight | 50 |
| Verify | MQTT=2,500, DLQ=500, total=3,000 |

### UC27: Intermittent 20% Send Failures

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 |
| Timeout | 180s |
| Sender | faultySender(20% transient) |
| Verify | All delivered eventually. DLQ empty. |

### UC28: Visibility Timeout Race

| Parameter | Value |
|-----------|-------|
| Volume | 500 |
| Timeout | 180s |
| SQS Visibility | 5s |
| Processor | variableDelayProcessor(0-8s) |
| AutoExtend | true |
| Verify | All 500 unique payloads arrive (at-least-once). |

### UC29: Message TTL Expiry

| Parameter | Value |
|-----------|-------|
| Volume | 500 (via Inject API) |
| Timeout | 120s |
| ExpiresAt | Already past |
| OnExpired | ExpiredDLQ |
| Verify | DLQ=500 (expired). MQTT=0. |

### UC30: DLQ Overflow (100% Poison)

| Parameter | Value |
|-----------|-------|
| Volume | 5,000 (all poison) |
| Timeout | 120s |
| Verify | DLQ=5,000. Bridge healthy. |

### UC31: Outbox Replay Exhaustion

| Parameter | Value |
|-----------|-------|
| Volume | 100 |
| Timeout | 120s |
| MaxReplayAttempts | 3 |
| Sender | alwaysFailSender |
| Verify | DLQ >= 100 after 3 replays each. |

### UC32: Graceful Shutdown Under Load

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (stop after >= 500) |
| Timeout | 60s |
| Verify | Stop() returns nil. IsRunning=false. No goroutine leak. |

---

## F. Backpressure & Concurrency (UC33-UC37)

### UC33: MaxInFlight=1 (Serial)

| Parameter | Value |
|-----------|-------|
| Volume | 500 |
| MaxInFlight | 1 |
| Verify | concurrencyTracker.maxConcurrency == 1 |

### UC34: MaxInFlight=1000 (High Concurrency)

| Parameter | Value |
|-----------|-------|
| Volume | 10,000 |
| MaxInFlight | 1,000 |
| Verify | concurrencyTracker.maxConcurrency > 1 |

### UC35: GlobalMaxInFlight Shared Across 3 Routes

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (1,000 per route) |
| MaxInFlight | 100 per route |
| GlobalMaxInFlight | 50 total |
| Verify | Total concurrent <= 50 |

### UC36: Slow Consumer

| Parameter | Value |
|-----------|-------|
| Volume | 1,000 |
| Sender delay | 100ms per send |
| MaxInFlight | 20 |
| Verify | All 1,000 arrive. No OOM. |

### UC37: Burst-Then-Idle

```
T+0s:  Burst 1 (1,000 msgs)
T+15s: Idle
T+20s: Burst 2 (1,000 msgs)
T+35s: Idle
T+40s: Burst 3 (1,000 msgs)
```

| Parameter | Value |
|-----------|-------|
| Volume | 3,000 (3 bursts) |
| Verify | 3,000 total. Bridge healthy after each burst. |

---

## G. Outbox & Delivery Modes (UC38-UC41)

### UC38: Outbox Depth Limit

| Parameter | Value |
|-----------|-------|
| Volume | 500 |
| MaxOutboxDepth | 100 |
| Sender | pausableSender (paused mid-stream) |
| Verify | DLQ + MQTT >= 500. Outbox drains after resume. |

### UC39: AckAfterOutboxPersist

| Parameter | Value |
|-----------|-------|
| Volume | 2,000 |
| AckAfter | AckAfterOutboxPersist |
| Sender delay | 200ms |
| Verify | SQS source empty before drain completes. |

### UC40: Adaptive Drain Backoff

| Parameter | Value |
|-----------|-------|
| Volume | 1,500 (1,000 + 500 with 15s idle gap) |
| DrainStrategy | AdaptiveBackoff(100ms, 5s, 2.0) |
| Verify | Idle drain cycles < 50 (vs 150 for fixed poll). |

### UC41: Idempotent Outbox Persist

| Parameter | Value |
|-----------|-------|
| Volume | 200 |
| SQS Visibility | 3s |
| Processor | slowFirstN(4s, limit=50) |
| Verify | Exactly 200 unique in SQS-OUT. Outbox dedup prevents duplicates. |

---

## Shared Helper Types

Defined in `longrunning_helpers_test.go`:

| Type | Kind | Purpose |
|------|------|---------|
| `concurrencyTracker` | processor | Atomically tracks max concurrent Process calls |
| `faultySender` | wrapper | Returns transient errors on N% of calls |
| `slowProcessor` | processor | Configurable delay per message |
| `filterProcessor` | processor | Drops messages by predicate (ErrMessageFiltered) |
| `pausableSender` | wrapper | Can be paused/resumed to simulate outages |
| `slowSender` | wrapper | Adds fixed delay per Send call |
| `alwaysFailSender` | sender | Always returns transient error |
| `chainOrderProcessor` | processor | Appends stage to chain_order header |
| `noopReceiver` | receiver | Blocks until context cancel (for Inject tests) |
| `setupFIFOQueue` | helper | Creates SQS FIFO queue |
| `sendBulkToSQSFIFO` | helper | Sends to FIFO queue with group IDs |
| `pollSQSBodies` | helper | Polls SQS until expected count |
