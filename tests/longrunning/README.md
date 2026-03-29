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

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC1 | Clustered SQS->MQTT->SQS (5 bridges) | 5,000 | Each output queue gets 5,000. No dupes. |
| UC7 | SQS FIFO ordering through MQTT | 3,000 | 3,000 in SQS-OUT. Soft per-group ordering. |
| UC8 | Multi-protocol fan-out (2 MQTT + 1 SQS) | 2,000 | All 3 targets get 2,000. |
| UC9 | MQTT QoS 2 stress | 5,000 | 5,000 unique. Zero duplicates. |
| UC10 | HTTP Inject API to MQTT | 1,000 | 1,000 with stage_inject header. |
| UC11 | SQS to SQS direct (no MQTT) | 5,000 | 5,000 in SQS-OUT. |

---

## B. Cluster & Lease Coordination (UC3, UC12-UC16)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC3 | 3-instance cascading failover | 2,000 | 2,000 unique. DLQ empty. |
| UC12 | Rolling restart (zero downtime) | 3,000 | >= 3,000 unique. |
| UC13 | Split-brain recovery (crash + lease expiry) | 2,000 | >= 2,000 unique. Fencing prevents stale sends. |
| UC14 | Lease contention (10 instances) | 1,000 | Only 1 active per sample. >= 1,000 delivered. |
| UC15 | ConnectAfterLease deferred connect | 2,000 | Standby connects only after lease acquired. |
| UC16 | Multi-session cluster (2 independent leases) | 2,000 | Alpha=1,000, Beta=1,000. No cross-contamination. |

---

## C. Message Size & Shape (UC17-UC21)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC17 | Large payloads (200KB) | 500 | SHA256 round-trip integrity. |
| UC18 | Tiny payloads high throughput (10B) | 50,000 | All seq 0-49999 present. |
| UC19 | Mixed payload sizes (50B/10KB/100KB) | 3,000 | Count per size_class header. |
| UC20 | Header-heavy (50 headers per msg) | 1,000 | All headers preserved. |
| UC21 | Binary payload round-trip (0x00, 0xFF) | 1,000 | SHA256 integrity after base64. |

---

## D. Routing & Filtering (UC2, UC22-UC26)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC2 | MQTT content-routed fan-out to 3 SQS | 3,000 | 1,000 per factory queue. |
| UC22 | 10-rule MatchRule routing | 5,000 | 500 per rule, all accounted. |
| UC23 | Subject prefix routing (3 prefixes) | 3,000 | Correct routing per prefix. |
| UC24 | Dynamic address templates ({tenant}/{region}) | 3,000 | 1,000 per combo delivered correctly. |
| UC25 | Filter processor (90% drop) | 10,000 | 1,000 passed + 9,000 DLQ = 10,000. |
| UC26 | 5-stage processor chain | 2,000 | chain_order="p1,p2,p3,p4,p5". |

---

## E. Failure & Recovery (UC6, UC27-UC32)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC6 | Burst + poison DLQ | 3,000 | MQTT=2,500, DLQ=500. |
| UC27 | Intermittent 20% send failures | 3,000 | All delivered eventually. DLQ empty. |
| UC28 | Visibility timeout race (vis=5s, delay=0-8s) | 500 | All 500 unique (at-least-once). |
| UC29 | Message TTL expiry | 500 | DLQ=500. MQTT=0. |
| UC30 | DLQ overflow (100% poison) | 5,000 | DLQ=5,000. Bridge healthy. |
| UC31 | Outbox replay exhaustion (MaxReplay=3) | 100 | DLQ >= 100 after 3 replays. |
| UC32 | Graceful shutdown under load | 3,000 | Stop() returns nil. No goroutine leak. |

---

## F. Backpressure & Concurrency (UC33-UC37)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC33 | MaxInFlight=1 (serial) | 500 | maxConcurrency == 1. |
| UC34 | MaxInFlight=1000 (high concurrency) | 10,000 | maxConcurrency > 1. |
| UC35 | GlobalMaxInFlight across 3 routes | 3,000 | Total concurrent <= 50. |
| UC36 | Slow consumer (100ms/send) | 1,000 | All 1,000 arrive. No OOM. |
| UC37 | Burst-then-idle (3 bursts) | 3,000 | All delivered. Bridge healthy between. |

---

## G. Outbox & Delivery Modes (UC38-UC41)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC38 | Outbox depth limit (MaxDepth=100) | 500 | DLQ + MQTT >= 500. |
| UC39 | AckAfterOutboxPersist | 2,000 | SQS empty before drain completes. |
| UC40 | Adaptive drain backoff | 1,500 | Idle drain cycles < 50. |
| UC41 | Idempotent outbox persist | 200 | Exactly 200 unique in SQS-OUT. |

---

## H. Broker Resilience (UC42-UC51)

Tests use per-test `mqttlocal.BrokerInstance` containers with custom configs.

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC42 | Broker kill+restart (SharedOutbox) | 3,000 | >= 3,000 unique after restart. DLQ empty. |
| UC43 | Broker kill+restart (DirectHold) | 2,000 | >= 2,000 unique via SQS redelivery. |
| UC44 | Broker low inflight quota (max=5) | 2,000 | Zero loss despite quota pressure. |
| UC45 | SharedOutbox vs DirectHold under quota | 2×1,000 | SharedOutbox >= 1,000. DirectHold gap logged. |
| UC46 | Broker message_size_limit=1024 | 1,000 | 500 small delivered, 500 oversized DLQ'd. |
| UC47 | Broker max_queued_messages=100 | 2,000 | Bridge sends all. Collector < 2,000 (broker drops). |
| UC48 | Multi-hop pipeline + broker kill | 2,000 | SQS-OUT >= 2,000 after restart. |
| UC49 | SharedOutbox vs DirectHold, 3 flaps | 2×2,000 | SharedOutbox >= 2,000 despite flapping. |
| UC50 | Session expiry during processing | 100 | All delivered. Bridge reconnects. |
| UC51 | Persistent session recovery | 1,000 | Collector >= 1,000 after broker restart. |

**Expect FAIL** until RES-001 (autopaho reconnect) is fixed.

---

## I. SQS Resilience (UC52-UC56)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC52 | Visibility timeout expiry (no auto-extend) | 50 | unique >= 50, total > 50 (duplicates). |
| UC53 | Auto-extend under sustained load | 200 | unique >= 200, fewer duplicates than UC52. |
| UC54 | FIFO deduplication window | 500+500 | Collector = exactly 500. |
| UC55 | FIFO ordering preservation (5 groups) | 1,000 | Per-group monotonically increasing. |
| UC56 | Batch mixed success/failure | 1,000 | 800 delivered + 200 DLQ = 1,000. |

---

## J. Outbox & Lease Edge Cases (UC57-UC62)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC57 | Stale claim recovery after crash | 1,000 | Bridge-B recovers all. DLQ empty. |
| UC58 | Double-drain prevention (fencing) | 2,000 | >= 2,000 unique. Only 1 active at each sample. |
| UC59 | Partition hotspot (single partition) | 5,000 | All delivered. Throughput logged. |
| UC60 | Outbox + broker down (AckAfterOutboxPersist) | 2,000 | SQS-IN empty. Collector = 2,000 after restart. |
| UC61 | MaxReplayAttempts with intermittent failures | 500 | All delivered (sender fails first 3, succeeds 4th). |
| UC62 | Lease renewal under high load | 10,000 | All delivered. DLQ empty. |

---

## K. Performance & Stability (UC63-UC68)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC63 | Memory stability | 50,000 | Final heap <= 2× initial. Max < 500MB. |
| UC64 | Latency percentiles | 10,000 | P50 < 500ms, P95 < 2s, P99 < 5s. |
| UC65 | Throughput ceiling (4 batches) | 36,000 | All delivered. Max msgs/sec logged. |
| UC66 | Multi-tenant isolation (10 tenants) | 5,000 | Tenants 1-9 < 30s each. Tenant 0 < 120s. |
| UC67 | Concurrent Reconcile during flow | 3,000 | >= 3,000 unique. No races. |
| UC68 | 5-minute soak (100 msgs/sec) | ~30,000 | >= 95% delivered. Heap < 2×. Goroutines stable. |

---

## L. DLQ & Error Paths (UC69-UC73)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC69 | DLQ replay integration | 500 | Phase 1: DLQ=500. Phase 2: collector=500. |
| UC70 | Error classification accuracy | 300 | 100 succeed + 200 DLQ (100 perm + 100 transient). |
| UC71 | Poison message exact attempt count | 100 | DLQ >= 100. Each Attempts >= 1. |
| UC72 | DLQ entry field integrity | 500 | All fields non-empty: ID, RouteID, Category, etc. |
| UC73 | Mixed error types in same stream | 1,000 | Collector >= 600, DLQ ~400. |

---

## M. Protocol Edge Cases (UC74-UC79)

| Test | Description | Volume | Key Assertion |
|------|-------------|--------|---------------|
| UC74 | MQTT retained messages | 101 | Collector = 101 (1 retained + 100 normal). |
| UC75 | Wildcard subscription overlap | 500 | Documents dedup vs double-delivery. |
| UC76 | MQTT QoS 0 fire-and-forget | 5,000 | No bridge errors. Collector > 0. Loss % logged. |
| UC77 | QoS 2 under broker restart | 1,000 | >= 1,000 unique. Zero duplicates. |
| UC78 | HTTP/SSE client disconnect | — | SKIP (needs HTTP factory infrastructure). |
| UC79 | SQS FIFO multi-group concurrent | 1,000 | Per-group ordering preserved. |

---

## N. Resilience Gap Validation (TEST-RES-*)

Tests that expose specific production code bugs. Each documents
the gap with `EVIDENCE` log lines.

| Test | Gap | What it Proves |
|------|-----|----------------|
| RES-001 | No circuit breaker on MQTT sender | Degraded sender stalls all slots (no fail-fast). |
| RES-003 | MQTT source drop without DLQ | 100 messages silently lost. No retry, no DLQ. |
| RES-005 | Auto-extend failure duplicates | SQS redelivers during processing → duplicates. |
| RES-006 | DLQ write blocks semaphore | Slow DLQ serializes all processing (no bulkhead). |
| RES-011 | Router panic swallows messages | Panicked messages lost. No DLQ, no log. |

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
| `errorClassSender` | wrapper | Returns error class based on header value |
| `noopReceiver` | receiver | Blocks until context cancel (for Inject tests) |

Defined in `longrunning_perf_helpers_test.go`:

| Type | Kind | Purpose |
|------|------|---------|
| `latencyRecorder` | processor | Records per-message latency, provides P50/P95/P99 |
| `heapSampler` | sampler | Background goroutine sampling runtime.ReadMemStats |
| `tenantSlowProcessor` | processor | Adds delay for a specific tenant (by header) |

Defined in `longrunning_fault_helpers_test.go`:

| Type | Kind | Purpose |
|------|------|---------|
| `failFirstNSender` | wrapper | Fails first N attempts per msg ID, then succeeds |
| `countingSender` | wrapper | Counts successes/failures separately |
| `degradedSender` | wrapper | Configurable failure rate % + latency injection |
| `slowDLQStore` | store | Configurable DLQ write delay (bulkhead testing) |
| `replayableDLQStore` | store | Extends lrDLQStore with working List/Replay |
| `rejectEveryNthProcessor` | processor | Rejects every Nth message (permanent error) |
| `panicProcessor` | processor | Panics on every Nth message |
