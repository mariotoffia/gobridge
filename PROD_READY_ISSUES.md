# Production Readiness Issues

Generated: 2026-06-30

Overall verdict: not production ready for the documented production/cluster/zero-loss posture.

The architecture has useful primitives: typed plugin config, ports/adapters separation, route policies, leases, shared outbox, DLQ, health probes, and observability hooks. The blockers are at the production seams: ACK boundaries, DLQ durability, outbox reclaim, cluster failover, live reconfiguration, docs/code drift, and several plugin-specific settlement/reconnect bugs.

## Cross-cutting blockers

- CRITICAL message durability: several paths ACK/complete before the durable target or DLQ write is guaranteed (`runtime/route/dispatch.go`, `runtime/dlq/router.go`, MQTT Paho delivery, Service Bus auto-renew). This can lose messages or failed-message evidence.
- CRITICAL docs/code drift: multiple advertised YAML examples do not match strict typed plugin config decoding, especially MQTT and Azure Service Bus.
- CRITICAL cluster safety: lease ownership does not reliably stop source consumption for MQTT, and cluster-wide reconfiguration has no version barrier or rollback semantics.
- HIGH failover: 30-60s failover is configurable in places but not the default or primary documented production posture. Several plugins rely on external LB/DNS failover without saying so.
- HIGH observability: failed DLQ writes, telemetry export loss, CloudWatch drops, config watcher drops, tenant/filter drops, and some lock/lease failures are not consistently reported.
- HIGH deployment/operator: the AWS filebased CDK ALB routes health and HTTP receiver traffic to the wrong port/path.

## Production question summary

1. Code production ready: FAIL. Concrete critical/high defects found.
2. Documentation production ready: FAIL. Several examples and endpoint/path claims contradict code.
3. Zero bugs: FAIL. The audit found concrete correctness bugs; zero bugs cannot be claimed.
4. Can run in a cluster: PARTIAL. Leases/outbox exist, but ownership, readiness, and rollout semantics are incomplete.
5. Resilient to outages and recovery: PARTIAL. Some reconnect/retry exists; stuck claimed records, unsafe DLQ, and plugin retry bugs remain.
6. Message loss recorded/reported/handled: FAIL. Some loss paths ACK before durable DLQ/outbox completion and do not leave reliable records.
7. Easy standard process/Docker/Kubernetes/ECS consumption: PARTIAL/FAIL. The demo CLI and AWS deployment docs do not provide a clean production path for all documented configs.
8. Runtime reconfiguration resilient: PARTIAL. Local rollback exists, but state reporting and in-flight semantics are flawed.
9. Cluster reconfiguration resilient: FAIL. No cluster-wide config version barrier or coordinated rollout.
10. Cluster failover in configurable 30-60s: PARTIAL. Tunable examples exist; defaults/docs and plugin behavior do not make it a reliable promise.

---

## Chunk: Core runtime, cluster, outbox, DLQ, dynamic reconfiguration

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, configuration/deployment/scenario docs, `TESTS.md`, `runtime/**`, `bridge/**`, `config/**`, `validate/**`, `ports/**`, native/DynamoDB outbox stores, runtime/bridge/e2e/long-running tests.

Verdict: not production ready. Durable/shared-outbox and DLQ semantics do not match the documented zero-loss posture.

Findings:

- CRITICAL resilience/message-loss: `runtime/route/helpers.go:91`, `runtime/bridge_start.go:112`, `runtime/bridge_start.go:148`, `docs/scenarios/05-durable-shared-outbox.md:111` - SharedOutbox records for documented MQTT to SQS route-session wiring are persisted under the binding partition while the drainer polls the route-session partition. Source can be ACKed after persist and the outbox record never drains. Fix partitioning or create binding-partition drainers; add a Scenario 5 regression test.
- CRITICAL correctness/message-loss: `runtime/route/dispatch.go:427`, `runtime/route/dispatch.go:435`, `domain/routing/policy.go:48`, `domain/routing/policy.go:194`, `docs/scenarios/05-durable-shared-outbox.md:142` - `ack_after: target_accept` is modeled and documented but ignored by `sharedOutbox`, which always ACKs after `Persist`. Operators selecting strongest ACK semantics do not get them. Implement target-accept correlation or reject/document it as unsupported for SharedOutbox.
- CRITICAL resilience/message-loss: `runtime/dlq/router.go:172`, `runtime/dlq/router.go:207`, `runtime/dlq/router.go:291`, `runtime/outbox/retry.go:91`, `runtime/outbox/retry.go:109`, `runtime/route/dispatch.go:156` - Terminal DLQ paths treat async enqueue as success, then ACK/complete source/outbox before the DLQ write is durable. Require confirmed DLQ write before terminal settlement, or keep source/outbox unsettled until DLQ persistence succeeds.
- HIGH resilience: `runtime/outbox/retry.go:115`, `runtime/outbox/retry.go:122`, `ports/stores.go:35`, `adapters/native/store/memoryoutbox/store.go:130`, `adapters/native/store/sqliteoutbox/acl_query.go:76` - Transient send failures leave records claimed and rely on stale-claim reclaim; memory/SQLite do not implement the same time-stale behavior used by tests. Add explicit release/retry state or consistent stale reclaim in production stores and conformance tests.
- HIGH correctness: `runtime/validator.go:113`, `runtime/validator.go:125`, `runtime/dlq/router.go:188`, `docs/configuration-reference.md:144` - Docs require a DLQ store for DLQ policies, but validation only requires it for non-retrying sources and `Router.Route` no-ops without a store. Permanent failures can be ACKed/completed without DLQ records. Validate DLQ store whenever policy routes terminal failures to DLQ, or require explicit `drop`.
- HIGH architecture/resilience: `bridge/supervisor.go:317`, `bridge/supervisor.go:363`, `docs/scenarios/10-dynamic-reconfiguration.md:11`, `ARCHITECTURE.md:1201` - Reconfiguration is per-process runtime swapping, not cluster-coordinated rollout. Cluster instances can run different route/store/session definitions with no version barrier or cluster rollback. Document this or add shared config-version coordination.
- MEDIUM documentation: `docs/scenarios/05-durable-shared-outbox.md:3`, `:90`, `:167` - Scenario 5 promises "zero message loss" while the main config uses memory stores and only later warns memory loses records on restart. Make durable examples use SQLite/DynamoDB by default; demote memory to dev-only.
- MEDIUM documentation/resilience: `docs/scenarios/08-clustered-exclusive-sessions.md:269`, `:382`, `runtime/session/config.go:17` - Cluster failover can be tuned to 60s, but defaults and main scenario are 300-360s. Provide an explicit production fast-failover profile.
- MEDIUM test-gap: `runtime/shared_outbox_transient_recovery_test.go:71`, `:173`, `runtime/fakes_test.go:447`, `TESTS.md:45` - Transient recovery tests depend on fake stale reclaim and sleeps. Move to store conformance/integration coverage against real stores and fake clocks/events.

Question coverage: Q1 FAIL, Q2 PARTIAL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL, Q6 FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 FAIL, Q10 PARTIAL.

---

## Chunk: Documentation, CLI, HTTP API, deployment/operator surface

Examined: `README.md`, `LANGUAGE.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `cmd/gobridge/main.go`, `httpapi/**`, `deployment/aws-filebased-config/**`, `docs/deployment-guide.md`, `docs/configuration-overview.md`, `docs/configuration-reference.md`, `docs/credentials-and-http-api.md`, `docs/aws-deployment/*.md`, `spec/httpapi/*.yaml`.

Verdict: not production ready. The documented/operator-facing deployment path has blocking health/routing drift and unsafe or misleading reconfiguration/cluster claims.

Findings:

- CRITICAL deployment: `deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment/attachment.go:230-231`, `:443-477`, `:480-512` - CDK ALB attachment routes `/healthz`/`/readyz` and HTTP receiver paths to the admin port, while monitor health is under `/api/v1/monitor/*` and HTTP transport uses `transport_http_addr`. ECS ALB health checks and receiver traffic can 404. Target monitor/transport ports correctly or add aliases on the targeted port.
- HIGH deployment: `docs/aws-deployment/configuration.md:63`, `deployment/aws-filebased-config/infra/bootstrap.go:41`, `deployment/aws-filebased-config/lib/bootstrap/app.go:141-169` - `node_role` is documented as controlling started components, but every node starts transport, admin, and monitor servers. Enforce `NodeRole` or document it as reserved/non-operative.
- HIGH correctness: `deployment/aws-filebased-config/lib/bootstrap/app.go:289-298`, `:159-160`, `httpapi/admin_config.go:30-42` - Rejected reloads are stored in `logicalRef` before apply, and `/api/v1/admin/config` returns that logical config as effective. Operators can see rejected config as active. Expose desired/applied/rejected state separately and read effective from `appliedRef`.
- HIGH documentation: `docs/deployment-guide.md:290-305`, `httpapi/monitor.go:75-93` - Kubernetes readiness example uses bare `/api/v1/monitor/ready`, but production gating likely needs `?level=connected|subscribed|full`. Document production readiness with the stricter level.
- HIGH resilience: `runtime/session/config.go:17-31`, `docs/configuration-reference.md:343-346`, `docs/scenarios/08-clustered-exclusive-sessions.md:256-269` - Default/primary cluster failover windows are 300-360s, not 30-60s. Provide a 30-60s HA profile or stop implying fast failover as default.
- HIGH correctness: `httpapi/server.go:383-386`, `httpapi/admin_config.go:23-27`, `spec/httpapi/http-api.yaml:883-918` - CORS allows only `GET, POST, OPTIONS`, but config transactions require `PATCH` and `DELETE`. Browser admin clients fail preflight. Add `PATCH, DELETE` or derive allowed methods from routes.
- MEDIUM deployability: `cmd/gobridge/main.go:85-100` - Example binary registers only MQTT and native memory/SQLite stores; SQS/DynamoDB are commented guidance. Mark it as demo-scoped or provide a production build profile.
- MEDIUM documentation: `docs/credentials-and-http-api.md:257-267`, `:345-358`, `spec/httpapi/http-api.yaml:425-433`, `httpapi/admin.go:31-34` - Docs say `POST /api/v1/admin/dlq/replay`, max 1000; code/spec use `/api/v1/admin/dlq/redrive`, max 100. Align docs with OpenAPI/code.
- MEDIUM correctness: `httpapi/admin_config.go:52-67` - Invalid JSON on config transaction create is silently ignored and a default transaction is created. Return `400` for non-empty malformed bodies.
- MEDIUM documentation: `docs/aws-deployment/configuration.md:97-103`, `docs/aws-deployment/overview.md:121-129` - AWS docs conflict on `config_file_path`: access-point path vs container mount path. Standardize on the container-visible path.

Question coverage: Q1 FAIL, Q2 FAIL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL, Q6 PARTIAL, Q7 FAIL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL.

---

## Chunk: MQTT Paho transport plugin

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/transport-configuration.md`, scenarios 01/03/08, `adapters/mqtt/transport/paho/**`, relevant `ports/transport.go`, `domain/messaging/headers.go`, `runtime/session/**`, `runtime/route/**`.

Verdict: not production ready. It is a useful MQTT client foundation, but at-least-once delivery, failover, backpressure, and documented config are not defensible.

Findings:

- CRITICAL message-loss: `adapters/mqtt/transport/paho/delivery.go:10`, `adapters/mqtt/transport/paho/acl_router.go:87`, `runtime/route/dispatch.go:427`, `docs/scenarios/08-clustered-exclusive-sessions.md:205` - MQTT `Ack` is a no-op because Paho acknowledges before application ownership, while docs claim SharedOutbox ACKs after persistence. A crash between broker ACK and outbox persist loses messages. Use an MQTT receive path with application-controlled ACK/settlement, or downgrade guarantees and docs.
- CRITICAL resilience/backpressure: `ports/transport.go:29`, `adapters/mqtt/transport/paho/receiver.go:53`, `adapters/mqtt/transport/paho/acl_router.go:87` - The port requires serial synchronous `emit`, but the router spawns a goroutine per inbound publish and returns immediately. `max_in_flight` cannot stop broker read-ahead, causing unbounded goroutines/memory and early ACKs. Route synchronously or through a bounded queue tied to `ReceiveMaximum`.
- CRITICAL cluster/message-loss: `runtime/session/manager_lease.go:45`, `runtime/session/manager_lease.go:216`, `docs/scenarios/08-clustered-exclusive-sessions.md:197` - Lease loss does not disconnect/unsubscribe/stop the MQTT receiver. A non-owner can keep consuming and ACKing during failover. On step-down, stop accepting source messages and close/disconnect the session.
- HIGH documentation/config: `config/parser/parse.go:296`, `adapters/mqtt/transport/paho/config_plugin.go:16`, `docs/scenarios/01-mqtt-to-mqtt.md:35`, `docs/transport-configuration.md:89` - Docs show flat MQTT options and receiver/sender `session_id` only, but strict typed decoding expects nested session/sender fields and receiver/sender transport kind. Update docs or decoder.
- HIGH cluster/docs: `adapters/mqtt/transport/paho/acl_session.go:171`, `docs/scenarios/08-clustered-exclusive-sessions.md:94`, `:269` - Scenario requires unique client IDs per instance but claims broker-retained QoS messages reach the new active instance. MQTT persistent queues are tied to client/session identity. Define one identity strategy and test it.
- HIGH security/metadata: `domain/messaging/headers.go:48`, `runtime/route/address.go:121`, `adapters/mqtt/transport/paho/acl_headers.go:251` - MQTT egress serializes internal `x-bridge.*` headers as user properties. Strip internal-only headers unless explicit bridge-to-bridge mode allows them.
- MEDIUM resilience: `adapters/mqtt/transport/paho/acl_session.go:117`, `runtime/session/manager.go:227` - Paho `OnConnectionUp` reconciles subscriptions, then session manager reconciles again. Pick one owner for reconnect reconciliation.
- MEDIUM readiness: `adapters/mqtt/transport/paho/session_health.go:12`, `:57` - `Ready` means connected only, while subscription/handler readiness is in `ServiceLevel`. Production probes should use full service level for receiver sessions.
- MEDIUM documentation: `docs/scenarios/01-mqtt-to-mqtt.md:199`, `docs/scenarios/03-mqtt-to-sqs.md:189`, `adapters/mqtt/transport/paho/delivery.go:10` - Docs overstate QoS/retain as end-to-end guarantees. Document MQTT QoS as broker-client packet semantics only.
- MEDIUM test-gap: `TESTS.md:21`, `docs/scenarios/08-clustered-exclusive-sessions.md:205` - Missing production tests for broker restart, crash before outbox persist, lease loss, and bounded 30-60s failover.

Question coverage: Q1 FAIL, Q2 FAIL, Q3 FAIL, Q4 FAIL, Q5 PARTIAL, Q6 FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL.

---

## Chunk: AWS SQS transport plugin

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/transport-configuration.md`, scenarios 02/03/05, `adapters/aws/transport/sqs/**`, relevant `ports/**`, `runtime/validator.go`, `runtime/route/**`, `runtime/outbox/**`, `runtime/bridge_start.go`.

Verdict: partial. Basic standard SQS send/receive is credible; FIFO, durable, clustered, and outage readiness are not production ready.

Findings:

- HIGH documentation/config: `docs/scenarios/02-sqs-to-sqs.md:52`, `docs/scenarios/03-mqtt-to-sqs.md:75`, `docs/scenarios/05-durable-shared-outbox.md:114`, `adapters/aws/transport/sqs/sender.go:84` - Scenario configs use queue names as binding addresses, but sender rejects non-empty address not equal to queue URL. Fix docs and add build-time validation.
- HIGH correctness: `adapters/aws/transport/sqs/factory.go:68`, `bridge/builder_complete.go:123`, `runtime/validator.go:138` - Visibility validation always uses 30s and ignores configured `visibility_timeout`. Thread actual receiver config into `SourceVisibilityTimeout`.
- HIGH correctness/panic: `adapters/aws/transport/sqs/acl_delivery.go:198`, `:227`, `:347` - `Extend()` can store `0`/`1` seconds while auto-extend later calls `ticker.Reset(0)`, which can panic. Clamp values and test boundary.
- HIGH concurrency: `adapters/aws/transport/sqs/acl_credentials.go:67`, `:84`, `adapters/aws/transport/sqs/config.go:29`, `adapters/aws/transport/sqs/acl_inbound.go:38` - Credential rotation swaps `client` under mutex while send/receive read it unlocked. Use atomic client snapshots/RWMutex and race tests.
- HIGH correctness/FIFO: `adapters/aws/transport/sqs/config.go:34`, `runtime/route/runner.go:199`, `docs/scenarios/02-sqs-to-sqs.md:146` - FIFO source ordering is not preserved when messages from the same group process concurrently. Serialize per `MessageGroupId` or force FIFO receivers to `max_messages: 1`.
- HIGH resilience: `runtime/outbox/retry.go:115`, `runtime/outbox/loop.go:214`, `domain/persistence/outbox.go:183`, `docs/scenarios/05-durable-shared-outbox.md:208` - Transient SQS send failure returns nil without completing or releasing the outbox record. Add failed/release transition or reliable stale reclaim.
- HIGH metadata: `adapters/aws/transport/sqs/config.go:223`, `adapters/aws/transport/sqs/acl_inbound.go:173`, `adapters/aws/transport/sqs/doc.go:24` - Docs say bridge headers map to SQS attributes, but code strips reserved `x-bridge.*`, losing idempotency/correlation across bridge hops. Preserve safe bridge-to-bridge headers or document loss.
- MEDIUM performance/docs: `runtime/route/dispatch.go:82`, `runtime/outbox/retry.go:53`, `docs/scenarios/02-sqs-to-sqs.md:19` - Runtime never uses `SendBatch`, so `batch_size` does not reduce normal bridge API calls. Implement BatchSender-aware dispatch/draining or document it as direct API only.
- MEDIUM capability: `adapters/aws/transport/sqs/factory.go:56` - Capabilities omit `shared_consumer` and `delayed_send`. Add or document the omission.
- MEDIUM operability: `adapters/aws/transport/sqs/config.go:52`, `config_plugin.go:32` - Poll backoff knobs exist internally but are not exposed in plugin config, so outage/failover tuning is not deployable. Expose and document them.
- MEDIUM correctness: `adapters/aws/transport/sqs/config.go:125`, `:212` - All headers become SQS attributes without SQS 10-attribute/name/size guards. Enforce limits or pack overflow deterministically.
- LOW documentation: `docs/transport-configuration.md:62` - Docs say FIFO dedup hashes logical subject only; code hashes payload, subject, ID, and creation time. Align docs.

Question coverage: Q1 FAIL, Q2 FAIL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL/FAIL, Q6 FAIL, Q7 PARTIAL, Q8 FAIL, Q9 FAIL, Q10 PARTIAL/FAIL.

---

## Chunk: Azure Service Bus transport plugin

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/transport-configuration.md`, `docs/scenarios/11-multi-tenant-azure-servicebus.md`, `docs/scenarios/05-durable-shared-outbox.md`, `adapters/azure/transport/servicebus/**`, relevant `ports/**`, `runtime/route/dispatch.go`, `runtime/validator.go`, `bridge/builder_complete.go`.

Verdict: not production ready. Queue happy path is credible; topic retry, lock lifecycle, documented config, and timeout/retry semantics are not production-safe.

Findings:

- CRITICAL documentation/config: `docs/transport-configuration.md:299-311`, `docs/scenarios/11-multi-tenant-azure-servicebus.md:48-59`, `adapters/azure/transport/servicebus/config_plugin.go:18-21`, `config/parser/raw_config.go:60-63` - Docs show flat ASB options, but strict decoder expects nested `receiver`, `sender`, and `connection`. Update docs or support the documented flat shape.
- CRITICAL correctness/message-duplication: `receiver.go:167-174`, `receiver.go:68-73`, `acl_inbound.go:212-230`, `runtime/route/dispatch.go:153`, `:267` - Delayed retry from a topic subscription republishes to the topic, causing retry fan-out to sibling subscriptions. Disable delayed retry for topic/subscription receivers or implement subscription-safe retry.
- CRITICAL resilience/message-loss: `ports/transport.go:36-42`, `acl_inbound.go:144-173`, `receiver.go:224-226` - If `emit` fails, auto-renew can keep the message invisible after `Run` returns. Stop auto-extension before returning emit failure when ownership is not transferred.
- HIGH correctness: `docs/scenarios/11-multi-tenant-azure-servicebus.md:230-235`, `acl_inbound.go:181-191`, `:202-249`, `:157-173` - `ReceiveAndDelete` is documented, but settlement still calls PeekLock-only operations. Make settlement/extension no-op for ReceiveAndDelete or reject unsupported mode.
- HIGH correctness: `acl_inbound.go:24-25`, `runtime/route/dispatch.go:321-330` - ASB delivery count is recorded but ignored; `max_replay_attempts` does not work for ASB. Expose receive count through Delivery or teach runtime to read ASB count.
- HIGH resilience: `acl_inbound.go:307-315`, `receiver.go:224-226` - Auto-extend failure cancels a private context, not the work context. Processing can continue after lock loss and race with redelivery. Propagate lock-loss cancellation/status to runtime.
- MEDIUM config/docs: `config.go:33-39`, `docs/transport-configuration.md:351-353`, `acl_inbound.go:335`, `acl_client.go:200-212` - `max_wait_time` and `prefetch` are configured/documented but not applied to the SDK. Wire them or remove docs.
- MEDIUM validation: `bridge/builder_complete.go:121-125`, `runtime/validator.go:131-142`, `factory.go:109-112` - Timeout-vs-lock validation is bypassed because ASB lacks `VisibilityTimeoutProvider`. Expose lock duration/default visibility or add ASB-specific route validation.
- MEDIUM docs/feature: `acl_client.go:21-26`, `runtime/route/dispatch.go:269-273` - Native ASB dead-letter is implied but not implemented; permanent failures route to GoBridge DLQ then ACK. Document "GoBridge DLQ only" or add native dead-letter policy.
- LOW test-gap: topic delayed retry and ReceiveAndDelete settlement behavior are not covered by integration tests.

Question coverage: Q1 FAIL, Q2 FAIL, Q3 FAIL, Q4 PARTIAL, Q5 FAIL, Q6 FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL.

---

## Chunk: RabbitMQ AMQP 0-9-1 transport plugin

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/transport-configuration.md`, scenarios 19/21, `adapters/amqp/transport/amqp091/**`, relevant `ports/transport.go`, `runtime/route/**`, `runtime/outbox/**`, `runtime/session/**`, selected AMQP tests.

Verdict: not production ready.

Findings:

- CRITICAL docs/runtime drift: `docs/transport-configuration.md:596`, `bridge/specs.go:20`, `bridge/convert.go:77`, `runtime/session/manager.go:147` - Declarative RabbitMQ topology in `topics[].options` is documented as auto-declared, but bridge never builds a `SessionPlan` from receiver topics/sender bindings. Runtime reconciles an empty/default plan. Assemble session plans from config and add scenario integration tests.
- CRITICAL message-loss: `acl_session.go:91`, `runtime/route/dispatch.go:97`, `acl_delivery.go:82`, `docs/transport-configuration.md:581` - `auto_ack` is exposed as normal receiver option while runtime still settles deliveries. With broker auto-ack, source messages are settled before downstream send succeeds. Reject `auto_ack=true` for managed routes or mark as unsafe/manual mode.
- HIGH resilience/backpressure: `factory.go:115`, `receiver.go:127`, `docs/transport-configuration.md:583` - Typed-config path loses default `prefetch_count: 10`; zero skips QoS, leaving unlimited prefetch. Apply defaults in receiver factory and test YAML/typed config omission.
- HIGH resilience: `receiver.go:91`, `:120`, `:251` - Consumer/channel errors while session remains connected immediately loop because `waitForReconnect` returns true on connected health. Back off, trigger reconcile, distinguish permanent consume errors, or fail component.
- HIGH correctness: `config_plugin.go:47`, `acl_outbound.go:245`, `docs/transport-configuration.md:592` - `immediate=true` is documented/exposed, but RabbitMQ does not support it and closes the channel. Reject/remove it.
- HIGH correctness/config: `config.go:29`, `config.go:125`, `acl_client.go:117`, `docs/scenarios/19-rabbitmq-to-rabbitmq.md:173` - `vhost` is parsed/documented but never applied to the broker URL. Inject it into URI path or delete the option/docs.
- HIGH shutdown: `acl_session.go:104`, `acl_session.go:119`, `receiver.go:171` - Delivery forwarding uses unbuffered send with no cancellation select. Shutdown under load can leak goroutines and hold SDK delivery state. Select on send vs cancellation.
- MEDIUM resilience: `sender.go:190`, `session.go:605`, `session.go:688` - Sender caches channel across reconnect; first send after reconnect can hit stale channel. Subscribe to session events or validate cached channel against current connection.
- MEDIUM observability/backpressure: `acl_client.go:14`, `session.go:383` - No RabbitMQ `NotifyBlocked`/`NotifyFlow` in health/metrics. Broker memory/disk alarms look like send timeouts. Add blocked/unblocked observation.
- MEDIUM cluster/failover: `config.go:20`, `docs/transport-configuration.md:562` - Single broker URL only; no documented RabbitMQ node failover contract. Document LB/DNS requirement or support URL rotation.
- MEDIUM config/topology: `acl_session.go:39`, `acl_session.go:47`, `config_plugin.go:56` - Topology declaration supports only basic exchange/queue flags and nil arguments. Production RabbitMQ features cannot be declared safely. Expose arguments or document pre-provisioned topology.

Question coverage: Q1 FAIL, Q2 FAIL, Q3 FAIL, Q4 PARTIAL/FAIL, Q5 PARTIAL, Q6 FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL/FAIL.

---

## Chunk: AMQP 1.0 transport plugin

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/transport-configuration.md`, scenarios 20/21, `adapters/amqp/transport/amqp10/**`, `runtime/route/dispatch.go`, `runtime/outbox/**`, `ports/transport.go`.

Verdict: not production ready; partial at best. Send/receive/settlement paths exist, but reconnect health, retry delay semantics, shutdown cancellation, and broker support claims are not production-safe.

Findings:

- HIGH resilience: `adapters/amqp/transport/amqp10/session.go:491-522` - `Conn.Done()` wakeups do not clear `s.conn`; `tryReconnect` sees non-nil conn and skips reconnect, leaving false `Connected=true` and potentially spinning. On conn done, mark disconnected, close/clear conn/session, emit disconnected, then reconnect.
- HIGH correctness/retry: `adapters/amqp/transport/amqp10/acl_delivery.go:136-146`, `runtime/route/dispatch.go:153`, `:267` - `Retry(ctx, after>0)` ignores actual delay while runtime passes meaningful backoff. Implement/document broker-specific delayed redelivery or return `ErrNotSupported`.
- HIGH shutdown: `adapters/amqp/transport/amqp10/receiver.go:95-108` - Receiver shutdown closes link with `context.Background()` and no timeout. Use bounded `LinkCloseTimeout` and caller cancellation.
- MEDIUM readiness: `adapters/amqp/transport/amqp10/session.go:315-319`, `runtime/bridge_start.go:137-139` - Health reports subscriptions active when connected, regardless of receiver link state. Track receiver link active/error state.
- MEDIUM domain invariant: `adapters/amqp/transport/amqp10/acl_outbound.go:41-45`, `:122-134` - Outbound `amqp10.subject` header can set AMQP Subject when `Envelope.Subject` is empty. Make `Envelope.Subject` the only egress source.
- MEDIUM docs/test: `adapters/amqp/transport/amqp10/README.md:3`, `integration_test.go:1`, `:15` - Docs claim Solace/Qpid testing, but visible integration is Artemis-local. Remove unsupported claims or add broker matrix tests.
- LOW failover/docs: `adapters/amqp/transport/amqp10/config.go:17-21`, `docs/transport-configuration.md:747-749` - Reconnect is same-endpoint only; no AMQP 1.0 multi-broker failover list. Document external LB/DNS requirement or add endpoint list support.

Question coverage: Q1 FAIL, Q2 PARTIAL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL, Q6 PARTIAL, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL/FAIL.

---

## Chunk: HTTP transport plugin

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/transport-configuration.md`, `docs/credentials-and-http-api.md`, scenarios 13/15, `docs/deployment-guide.md`, `docs/aws-deployment/http-api.md`, `spec/http-adapter/http-api.yaml`, `adapters/http/transport/**`, `deployment/aws-filebased-config/lib/bootstrap/transport_server.go`.

Verdict: partial. Acceptable for controlled single-node HTTP ingress behind a hardened gateway; not ready for clustered ingress/SSE.

Findings:

- HIGH cluster/security: `adapters/http/transport/receiver.go:108`, `:148` - Client-controlled `X-Bridge-Forwarded: true` bypasses route-location forwarding. A caller can force local processing on a non-owner node. Trust forwarded state only after peer authentication or use an internal token.
- HIGH metadata/security: `adapters/http/transport/sender_sse.go:140-145`, `:283-288`, `domain/messaging/headers.go:48-50`, `:106-113` - SSE serializes headers directly, including internal `x-bridge.*`. Strip internal-only headers before external SSE.
- HIGH deployment: `deployment/aws-filebased-config/lib/bootstrap/transport_server.go:63-66`, `adapters/http/transport/sender_sse.go:254-279` - Production transport server sets `WriteTimeout: 30s` while SSE is long-lived. Healthy SSE streams can be killed. Split ingress/SSE listeners or use per-write deadlines.
- HIGH resilience: `adapters/http/transport/sender_sse.go:269-278` - SSE writes have no per-frame write deadline and can block goroutines on slow clients. Set per-write deadlines and close slow clients.
- HIGH cluster/metadata: `adapters/http/transport/forwarder.go:149-170`, `receiver.go:226-230`, `domain/messaging/envelope.go:92-104` - Cluster forwarding puts `env.Headers()` in body, then receiver strips reserved headers and only trusts selected HTTP headers. Idempotency, tenant, and trace context can be lost across forwarding. Define authenticated bridge-to-bridge envelope propagation.
- MEDIUM correctness: `adapters/http/transport/receiver.go:118`, `:126-129` - Oversized bodies are returned as `400`, and decoder accepts first JSON value without EOF check. Return `413` for `MaxBytesError` and reject trailing tokens.
- MEDIUM security/config: `adapters/http/transport/config.go:22-27`, `:48-63`, docs scenario 15 - API key examples imply min-16, but `Config.Validate` does not enforce it. Enforce or document no minimum.
- MEDIUM auth: `adapters/http/transport/helpers.go:19-20`, `receiver.go:103-105`, `sender_sse.go:194-196` - 401 responses omit `WWW-Authenticate` for Bearer-capable endpoints. Add appropriate challenge.
- LOW docs: docs scenarios 13/15 use bare `http.ListenAndServe` without timeouts/TLS caveats. Mark as development-only or show hardened server/proxy setup.
- LOW test-gap: missing adversarial tests for forwarded-header spoofing, SSE internal-header leakage, trailing JSON, 413 mapping, slow SSE writers; one inspected test uses `time.Sleep`.

Question coverage: Q1 PARTIAL, Q2 PARTIAL, Q3 FAIL, Q4 PARTIAL/FAIL, Q5 PARTIAL, Q6 PARTIAL/FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL.

---

## Chunk: Native store/config/credential plugins

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/processors-and-stores.md`, `docs/config-stores.md`, `docs/credentials-rotation.md`, `docs/configuration-overview.md`, `docs/credentials-and-http-api.md`, `docs/configuration-reference.md`, `docs/scenarios/05-durable-shared-outbox.md`, `adapters/native/store/**`, `adapters/native/config/file/**`, `adapters/native/credentials/file/**`.

Verdict: no/partial. Memory plugins are dev/test only; SQLite DLQ is close for single-process use; SQLite outbox has a production-blocking crash-recovery gap; file config is usable with caveats; file credentials are not production-safe as a rotating secret store.

Findings:

- CRITICAL durability: `docs/scenarios/05-durable-shared-outbox.md:304-319`, `memorylease/store.go:51-60`, `:96-104`, `sqliteoutbox/acl_query.go:76-79`, `:16-40`, `:91-99`, `adapters/native/store/factory.go:58-65` - SQLite outbox can strand claimed records after crash while docs advertise SQLite+memory lease crash survival. Persist `claimed_at`, honor stale claim duration, add restart/crash tests, or stop documenting it as crash-safe.
- HIGH concurrency: `sqliteoutbox/acl_session.go:30-46`, `sqlitedlq/acl_session.go:29-45`, `sqliteoutbox/acl_query.go:91-99` - SQLite stores do not set `busy_timeout`, connection limits, or retry policy; claim updates lack status/version guards. Harden writer behavior and test concurrent claims/writes.
- HIGH security/durability: `repository.go:206-227`, `repository.go:356-364` - File credentials update rewrites target file directly. Crash/readers can see truncated/partial JSON secrets. Use temp file, fsync file/dir, chmod 0600, atomic rename, and locking/CAS where needed.
- HIGH observability/reconfig: `acl_watcher.go:161`, `acl_watcher.go:300-313` - File config watcher has one-slot channel and silently drops valid reloads. Coalesce safely or report dropped reloads.
- MEDIUM lifecycle: `sqliteoutbox/outbox.go:58-61`, `sqlitedlq/dlq.go:36-39`, `runtime/bridge.go:291-318` - Runtime does not close SQLite store handles on stop/reload. Add optional closer handling for stores.
- MEDIUM error/cancellation: `source.go:44-47`, `repository.go:143-264` - File config/credential operations mostly ignore ctx and return raw errors. Check ctx at boundaries and map errors to `BridgeError`.
- MEDIUM test-gap: `TESTS.md:266-320`, `tests/longrunning/uc57_outbox_lease_test.go:52-54` - Missing SQLite claimed-record crash recovery, concurrent writer/busy, file credential partial-write, and config watcher backpressure tests.
- LOW docs: durable examples overstate memory/SQLite production posture; `stale_claim_duration` is documented generically although native SQLite ignores it.

Plugin verdicts:

- Memory lease: not production ready; dev/test only.
- Memory outbox: not production ready; loses messages on restart.
- Memory DLQ: not production ready; loses evidence on restart.
- SQLite outbox: not production ready until claimed-record crash recovery is fixed.
- SQLite DLQ: partial single-process readiness; needs busy timeout/retry and lifecycle close.
- File config: partial; watcher can drop valid reloads.
- File credentials: not production-safe for rotating secrets; acceptable for local/dev or immutable mounted secrets.
- Native store aggregator: partial; ignores SQLite stale-claim runtime options and has no lifecycle ownership.

Question coverage: Q1 FAIL, Q2 PARTIAL/FAIL, Q3 FAIL, Q4 FAIL for native stores, Q5 PARTIAL, Q6 PARTIAL/FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 FAIL, Q10 FAIL.

---

## Chunk: AWS DynamoDB/config/credential/metrics plugins

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/processors-and-stores.md`, `docs/config-stores.md`, `docs/credentials-rotation.md`, `docs/aws-deployment/*.md`, scenarios 05/08/09, `adapters/aws/store/**`, `adapters/aws/config/dynamodb/**`, `adapters/aws/credentials/ssm/**`, `adapters/aws/metrics/cloudwatch/**`, deployment bootstrap/CDK/grants/validation.

Verdict: partial/no for clustered production. Conditional-write intent exists, but lease fencing lifetime, outbox completion consistency, config CAS/reload consistency, deployment wiring, and metrics/SSM behavior are not production-tight.

Findings:

- HIGH correctness/cluster: `adapters/aws/store/dynamodblease/acl_store.go:112-119`, `:243-246`, `:274` - Lease fencing token monotonicity can reset after DynamoDB TTL deletes a released lease row. Never TTL-delete fencing counter rows or split lease presence from non-expiring monotonic counter.
- HIGH message-duplication: `adapters/aws/store/dynamodboutbox/acl_store.go:423-431`, `:657-668` - `Complete` resolves keys through an eventually consistent GSI immediately after send. Lagging GSI can return not found and duplicate later. Complete by base-table keys from `Claim`, encode keys in ID, or bounded-retry GSI misses.
- HIGH reconfig/consistency: `adapters/aws/config/dynamodb/acl_params.go:25-32`, `:58-69`, `:90-99`, `loader.go:268-278` - Config reads are eventual and `Save` is unconditional local-version `PutItem`, so concurrent saves can lose updates and watchers reload stale data. Use strong reads and conditional writes or document/admin-gate last-writer-wins.
- HIGH deployability: `deployment/aws-filebased-config/lib/bootstrap/registry.go:42-43`, `cdk/constructs/internal/gobridgebase/grants.go:49-82` - AWS filebased runtime/CDK does not register DynamoDB store factory or grant table permissions. DynamoDB store configs will not run in this profile. Register/grant or reject with clear validation.
- HIGH scalability: `adapters/aws/store/dynamodboutbox/acl_store.go:280-289`, `:619-655` - Every `Claim` scans entire outbox partition to compute max claim version. Maintain per-partition fence metadata or redesign stale-token rejection.
- MEDIUM ops: `dynamodblease/acl_store.go:321-331`, `dynamodboutbox/acl_store.go:126-170`, `dynamodbdlq/acl_store.go:97-127` - Tables write TTL attributes but table creation never enables TTL; if enabled for leases it can break fencing, if disabled data accumulates. Provide explicit infra/table validation and different retention rules per store.
- MEDIUM durability: `adapters/aws/store/dynamodbdlq/acl_store.go:157-174` - DLQ default TTL is `failed_at + 1h`, too short for production investigation if TTL is enabled. Default to no TTL or configurable days-scale retention.
- MEDIUM security/resilience: `adapters/aws/credentials/ssm/repository.go:188-190`, `acl_errors.go:24-35` - SSM update/delete versioning is TOCTOU; throttling/auth/KMS/context errors collapse to unavailable. Classify errors and document admin writes as non-atomic or move rotation writes to CAS backend.
- MEDIUM observability: `adapters/aws/metrics/cloudwatch/acl_batcher.go:236-254`, `exporter.go:108-118` - Metrics are drained before `PutMetricData`; background flush drops errors and does not requeue. Requeue with bounded retry/drop counters and structured warnings.
- MEDIUM observability/cardinality: `adapters/aws/metrics/cloudwatch/acl_batcher.go:148-155`, `docs/aws-deployment/monitoring.md:184-199` - Dimensions are silently truncated at 30 and docs recommend high-cardinality `queue_url`. Validate/drop with warning and normalize dimensions.
- LOW scalability: `adapters/aws/store/dynamodbdlq/acl_store.go:316-360`, `:488-520` - Unfiltered DLQ list/purge uses full scans. Require indexed filters or bounded pagination.
- LOW docs: `docs/scenarios/09-layered-dynamodb-config.md:423-435`, `adapters/aws/config/dynamodb/acl_params.go:40-51` - Scenario says config item uses `config` map; implementation uses `data` string JSON. Align schema/docs.

Plugin verdicts:

- DynamoDB lease: partial; TTL deletion can break fencing token monotonicity.
- DynamoDB outbox: partial/no; GSI completion race and O(N) claim scan block production readiness.
- DynamoDB DLQ: partial; idempotent writes, but retention/scan semantics need production controls.
- DynamoDB config: partial/no; lacks strong consistency/CAS for production admin reloads.
- SSM credentials: partial; read path works, rotation/admin semantics and error classification weak.
- CloudWatch metrics: partial; exporter works, failure loss/cardinality need hardening.
- AWS store aggregator: partial; factory exists, deployment/runtime integration missing.

Question coverage: Q1 PARTIAL/FAIL, Q2 PARTIAL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL, Q6 PARTIAL/FAIL, Q7 FAIL for AWS filebased profile, Q8 PARTIAL/FAIL, Q9 PARTIAL/FAIL, Q10 PARTIAL.

---

## Chunk: OpenTelemetry metrics/tracing plugins

Examined: `adapters/otel/metrics/**`, `adapters/otel/tracing/**`, `observability/**`, `logging/**`, `runtime/route/runner.go`, `runtime/bridge.go`, `ports/{metrics,tracer}.go`, `domain/messaging/tracecontext.go`, `domain/shared/metrics.go`, `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `docs/scenarios/18-observability.md`, `docs/deployment-guide.md`, `docs/aws-deployment/monitoring.md`, `docs/scenarios/cdk/04-production-stack.md`, `TESTS.md`.

Verdict: partial. Metrics are usable for happy-path export; tracing is not production-ready for distributed diagnosis.

Findings:

- HIGH tracing: `runtime/route/runner.go:352`, `adapters/otel/tracing/acl_client.go:99` - W3C `traceparent` is parsed as attributes/log context, but not extracted into OpenTelemetry context before `StartSpan`. Use OTel `propagation.TraceContext` extraction before span creation and inject new child context onto outbound headers.
- HIGH lifecycle: `runtime/bridge.go:310`, `ports/tracer.go:20` - Runtime shutdown only calls `metrics.Flush`; tracer port has no `Close`. Tail spans can be lost and metric providers are not closed by runtime. Add managed telemetry lifecycle or explicit bounded close hooks.
- HIGH observability: `adapters/otel/metrics/acl_client.go:103`, `adapters/otel/tracing/acl_client.go:70` - Telemetry export/backpressure failures are effectively invisible. Add logger/error callback/self-metric path and expose batch/export timeout/queue settings.
- HIGH docs: `docs/scenarios/18-observability.md:44`, `domain/shared/metrics.go:40` - Docs/dashboard examples use `bridge.delivery.*` names the runtime does not emit. Generate docs from constants or add explicit name translation.
- HIGH docs/config: `docs/scenarios/cdk/04-production-stack.md:288`, `docs/scenarios/18-observability.md:125` - Deployment docs show YAML `tracing:` config while scenario says observability is configured in Go code. Implement config/env wiring or delete unsupported YAML.
- MEDIUM correctness: `adapters/otel/tracing/config.go:45`, `:73` - `WithSamplerRatio(0)` cannot disable tracing because zero is treated as unset and reset to 1.0. Track unset explicitly and validate `[0,1]`.
- MEDIUM deployability: `adapters/otel/metrics/config.go:89`, `adapters/otel/tracing/config.go:66` - Hardcoded defaults and no `OTEL_*` docs mean standard deployment env vars are not supported. Honor/document `OTEL_EXPORTER_OTLP_*`, `OTEL_SERVICE_NAME`, and resource attributes.
- MEDIUM test-gap: `adapters/otel/metrics/export_test.go:7`, `adapters/otel/tracing/export_test.go:7` - Tests avoid real collector/export-failure paths. Add `httptest`/collector tests for flush, outage, queue overflow, retry/drop visibility.
- LOW cardinality: `adapters/otel/metrics/acl_client.go:39` - Metric instruments cache arbitrary metric names with no bound/guidance. Document/reject dynamic names.

Plugin verdicts:

- Metrics: partial; emits instruments and flushes, but lifecycle ownership, docs names, env config, and export-failure visibility need fixes.
- Tracing: no; parent propagation, shutdown ownership, drop/backpressure visibility, and sampler semantics are not production-ready.

Question coverage: Q1 PARTIAL, Q2 PARTIAL/FAIL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL, Q6 FAIL for telemetry loss reporting, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL.

---

## Chunk: Processor plugins

Examined: `README.md`, `ARCHITECTURE.md`, `DDD.md`, `PLUGIN.md`, `TESTS.md`, `docs/processors-and-stores.md`, scenarios 04/06/14/17, `processors/filter/**`, `processors/transform/**`, `processors/tenant/**`, `processors/circuitbreaker/**`, `circuitbreaker/**`, `runtime/route/chain.go`, `runner.go`, `dispatch.go`, messaging/error primitives.

Verdict: not production ready as documented policy/security/resilience controls.

Findings:

- CRITICAL integration/resilience: `runtime/route/chain.go:99`, `runtime/route/runner.go:390`, `runtime/route/runner.go:418`, `processors/circuitbreaker/processor.go:63`, `docs/scenarios/06-transform-circuit-breaker.md:33` - Circuit breaker cannot observe sender failures. Processors run before dispatch; as last processor, `next` is terminal no-op. Move breaker into dispatch/sender wrapping or scope docs/API to processor-chain failures only.
- HIGH error-classification: `processors/transform/transform.go:86`, `:116`, `domain/shared/errors.go:327`, `docs/scenarios/06-transform-circuit-breaker.md:186` - Transform failures return plain `fmt.Errorf`, which runtime treats as retryable, contradicting docs saying rejected/permanent/DLQ. Wrap as `BridgeError`.
- HIGH correctness: `processors/transform/transform.go:63` - Invalid JSON payloads pass through unchanged even with mappings configured. Fail with structured rejected error when `FailOnError` or required mappings exist.
- HIGH validation: `processors/filter/filter.go:75`, `processors/filter/condition.go:135`, `processors/filter/filter_edge_test.go:14` - Unknown filter actions fall through and unknown operators become per-message plain errors. Validate action/operator at construction with structured setup errors.
- HIGH security/policy: `processors/filter/condition.go:87`, `processors/filter/filter.go:71`, `processors/filter/filter_edge_test.go:87` - JSON parse failure is silent no-match, allowing malformed JSON to bypass `ActionDrop` filters. Add strict JSON-path mode or classify parse failure as rejected.
- HIGH tenant/security: `processors/tenant/processor.go:25`, `domain/messaging/headers.go:27`, `runtime/route/runner.go:341`, `docs/processors-and-stores.md:277` - Tenant default header is reserved `x-bridge.tenant-id`, but runtime strips reserved headers at ingress. Use non-reserved default or document explicit `TenantHeader`.
- HIGH docs: `docs/scenarios/14-multi-tenant-priority-routing.md:397`, `processors/transform/transform.go:136`, `processors/tenant/processor.go:47` - Scenario claims transform can copy payload tenant into envelope header, but transform only writes JSON payload fields and tenant reads headers. Add header-mapping processor or correct recipe.
- HIGH resilience/load: `processors/circuitbreaker/processor.go:47`, `:55`, `:108` - High-cardinality keys can churn fixed 10k breaker cache and evict open breakers. Make capacity configurable/observable and avoid untrusted key spaces.
- MEDIUM timeout/mutation: `runtime/route/chain.go:131`, `processors/transform/transform.go:63`, `processors/filter/filter.go:62` - Runtime can return processor timeout while filter/transform continue CPU work and may mutate envelope later. Check `ctx.Err()` around parse/mapping/evaluation loops and add payload/path limits.
- MEDIUM observability: `processors/tenant/processor.go:80`, `:93`, `runtime/route/dispatch.go:254` - Tenant tracker decrement/message-count errors and filtered drops have little/no structured observability. Emit metrics/log hooks for rejects, drops, skipped mappings, and tracker failures.
- LOW test-gap: `processors/circuitbreaker/resilience_fixes_test.go:61`, `TESTS.md:45` - Circuit-breaker tests use `time.Sleep` despite repo test rules. Replace with fake clock/event synchronization.

Plugin verdicts:

- Filter: partial; works for simple routing, not safe as strict policy gate.
- Transform: no; invalid JSON and transform failures are misclassified/pass-through.
- Tenant: partial; usable with explicit non-reserved header and validator, unsafe default/docs.
- Circuit breaker: no; state machine is useful, but processor placement does not protect senders.

Question coverage: Q1 FAIL, Q2 FAIL, Q3 FAIL, Q4 PARTIAL, Q5 PARTIAL/FAIL, Q6 PARTIAL/FAIL, Q7 PARTIAL, Q8 PARTIAL, Q9 PARTIAL, Q10 PARTIAL.

---

## Minimum fix order

1. Fix message-loss blockers first: SharedOutbox partitioning, ACK boundary semantics, durable DLQ confirmation before ACK/Complete, MQTT receive ACK/backpressure, Service Bus topic retry/auto-renew.
2. Fix cluster safety: MQTT lease step-down stops consumption, cluster-wide config version coordination or explicit docs limitation, production 30-60s HA profile.
3. Fix deployment/doc truth: typed config examples, AWS ALB health/receiver routing, root CLI/demo labeling, DLQ endpoint docs, readiness level docs.
4. Fix plugin-specific settlement/reconnect bugs: AMQP091 auto-ack/topology/prefetch/vhost/immediate, AMQP10 conn-done reconnect and delayed retry semantics, SQS visibility/race/FIFO/outbox release, HTTP forwarded-header trust and SSE header/deadline behavior.
5. Fix observability/reporting: DLQ write failures, telemetry export drops, CloudWatch drops, config watcher drops, processor rejects/drops.

No code changes were applied by this audit.

---

## Post-remediation backlog — deferred follow-ups

_Added 2026-07-02. All 117 audit findings above were remediated across Waves 0–5 (`make lint` and `make test` green). The items below are the **48 deferred follow-ups** surfaced during implementation and adversarial review: lower-severity own-unit refinements or documented boundaries, none blocking production. Listed here so a fresh session can pick them up without the session database._

| Severity | Count |
| --- | --- |
| MEDIUM | 5 (✅ resolved) |
| LOW | 40 |
| NICE | 4 |

### MEDIUM (5)

> **✅ RESOLVED 2026-07-02** on branch `fix/prod-ready-medium-followups` (`make lint` + `make test` green).
> Each item below carries a **Resolution** note. One adjacent shape surfaced during the
> adversarial review of these fixes — a receiver whose `session_id` never gets a session
> **manager** (distinct from the planless-manager case P4 closes) — is tracked as a new
> follow-up: **`ADV-P4-FU1`** under LOW below.

#### `ADV-F1-P3` — amqp091
amqp091 producer exchange never auto-declared (PublisherPlan empty); publish to an absent custom exchange fails visibly (404), not silent

- **Files:** `bridge/specs.go`, `adapters/amqp/transport/amqp091/session.go`
- **Notes:** F1 left SessionPlan.Publishers empty (documented ponytail boundary). amqp091 declarePublisher reads exchange name solely from pub.Topic; that name lives in the opaque typed sender PluginConfig (amqp091.Config.Sender.Exchange) which the transport-neutral bridge cannot read without importing the adapter (arch-lint forbidden) or a new ports accessor. SenderDef/BindingDef carry routing key (Address), not exchange. Failure is VISIBLE: PublishConfirmed -> channel 404 -> mapPublishError -> retry/DLQ (sender.go:119-129), so no silent loss. Fix needs a transport-neutral exchange accessor (ports interface the adapter implements) so bridge can emit PublisherPlan{Topic: exchange} and pre-declare. Out of bridge-only scope. Related to existing F11.
- **Resolution (2026-07-02):** Added transport-neutral optional interface `ports.PublishingConfig { PublisherTopic() string }`; `amqp091.Config` implements it returning `Sender.Exchange`. `bridge/specs.go:sessionPlanFor` now emits a `PublisherPlan` (deduped by exchange, first-wins) for every sender bound to the session, so `declarePublisher` pre-declares the producer exchange. The declare is **best-effort / non-fatal**: on broker rejection (`PRECONDITION_FAILED` for a pre-existing exchange, `ACCESS_REFUSED` under least-privilege creds) the session logs a warning, increments `AMQP091PublisherDeclareFailed`, and continues — a pre-existing / externally-managed exchange is never taken down, and a genuinely-absent one still fails visibly at publish (404). `sender.exchange` names the declared exchange; the `publisher.*` block only decorates topology. Tests: `adapters/amqp/transport/amqp091/prodready_publisher_declare_test.go` (non-fatal publisher vs. fatal subscription asymmetry), `bridge/session_plan_test.go`. Docs: `docs/transport-configuration.md` (Publisher-side exchange auto-declare).

#### `HTTP-N1` — http
SSE per-write deadline silently no-ops when fronting middleware wraps ResponseWriter without Unwrap() -> slow-client pins handler goroutine (H4 failure returns)

- **Files:** `adapters/http/transport/sender_sse.go`
- **Notes:** NICE-TO-HAVE: probe SetWriteDeadline once at stream start, warn+metric when unsupported so slow-client eviction inertness is observable.
- **Resolution (2026-07-02):** Added `MetricSSEDeadlineUnsupported` counter (`adapters/http/transport/metrics.go`), emitted once per stream in `ServeHTTP` alongside the existing start-of-stream warn when `write_timeout > 0` but the (possibly middleware-wrapped) `ResponseWriter` exposes no working `SetWriteDeadline` (no `Unwrap()` chain reaching one). Slow-client eviction inertness is now observable, not silent. Test: `prodready_sse_deadline_test.go` (bare recorder increments the counter; a deadline-capable writer does not).

#### `C3-FU2` — runtime
Whole-runtime terminal on a single session lease blip: one session re-acquire failure cancels the entire runtime (all routes) rather than isolating the failed session. Consider per-session failure isolation.

- **Files:** `runtime/bridge.go`, `runtime/supervisor.go`
- **Notes:** Acceptable now: single-use re-acquire failure is rare (3 renew fails ~30s outage) and restart yields a clean fleet. Follow-up: per-session restart/quarantine to avoid blast radius.
- **Resolution (2026-07-02):** Session managers now run under `Runtime.superviseSession`, which isolates a single session's failure from the fleet. On `mgr.Run` error (ctx still live) it records the session in `componentErrors`, emits `MetricSessionRestarts`, and retries with capped-exponential backoff **plus equal-jitter** (`wait ∈ [backoff/2, backoff]`), resetting the backoff after a sustained-stability window and clearing the session's `componentErrors` entry on clean stop / before a successful retry. It never sets terminal or global `healthy=false`, so other routes keep running; panics still hit `startBackground`'s recover (documented boundary). Unbounded retry is deliberate — a terminal ceiling would reintroduce the exact whole-fleet outage this issue fixes; permanent failures instead surface via the continuous `MetricSessionRestarts` rate, `componentErrors`, and per-session readiness (`/ready?level=...`). Tests: `runtime/session_supervise_test.go` (fleet survival, backoff reset after sustained recovery, componentErrors cleared after recovery; all pass with `-race`).

#### `E5-FU1` — runtime/route+adapters
Transport redelivery-count headers (sqs./asb./amqp10.delivery-count) survive bridge-to-bridge hops and can win receiveCount precedence over the fresh local count, mis-firing max_replay_attempts in chained mixed-transport topologies (premature DLQ = good-message loss, or stale<cap = no cap).

- **Files:** `runtime/route/dispatch.go:355-363`, `adapters/azure/transport/servicebus/acl_outbound.go`, `adapters/amqp/transport/amqp10/acl_outbound.go`, `adapters/aws/transport/sqs`
- **Notes:** Discovered by E5 adversarial review (e5-adversarial). NOT a receiveCount bug — root cause is adapter ACLs carrying foreign transport count headers across hops (these keys are transport-namespaced, NOT x-bridge.* reserved, so unclassified + unstripped). Reachable: foreign count header carried at egress (servicebus/acl_outbound.go:91, amqp10/acl_outbound.go:117 pass non-own-prefix props) and re-imported at ingress (servicebus/acl_inbound.go:60-64, amqp10/acl_inbound.go:70-74). Repro: env {asb.delivery-count:9, amqp10.delivery-count:uint32(0)} -> receiveCount=9 not 1. E5 fix marginally widens a PRE-EXISTING class (sqs stale header already won before). Documented in dispatch.go:344-349 receiveCount comment. Two fix options: (A) strip sqs./asb./amqp10. count keys at egress in each adapter ACL (or do not re-import foreign ones at ingress); (B) thread source-transport identity from RouteRunner into receiveCount so it reads ONLY the local transport count (local ingress always overwrites its own key fresh; foreign keys are always stale). Related cluster: PROD_READY_ISSUES #91/#115/#205 (x-bridge.* hop hygiene). Own unit + review.
- **Resolution (2026-07-02):** Chose the runtime-chokepoint strip (Option A, but transport-agnostic and in one place) over threading source identity. New helper `stripInboundReceiveCounts(env)` (`runtime/route/dispatch.go`) deletes the three foreign redelivery-count keys (`sqs./asb./amqp10.delivery-count`); applied to the **outbound clone** in `sendDirectHold` and to a **pre-persist clone** in `buildOutboxRecords`, never to the source env (receiveCount is re-read on retry paths). Downstream hops now read only the fresh local count, so `max_replay_attempts` can no longer be tripped early (nor stuck below cap) by a stale cross-hop count, and the wire is cleaned of foreign bookkeeping. Test: `runtime/route/dispatch_test.go` (stale foreign count stripped; fresh local count preserved; nil-header safe).

#### `ADV-F1-P4` — validate
Receiver with topics on a session that only gets a planless session-sender manager silently subscribes to nothing (F1-class silent loss)

- **Files:** `validate/blueprint_graph.go`
- **Notes:** REACHABLE shape: shared_outbox route omits its Session block but a binding has session_id==receiver.SessionID; that session gets only a planless RegisterSessionSender manager (bridge_start.go:194-199) so a plan-driven receiver on it subscribes to nothing. Pre-existing, NOT introduced by F1. CAVEAT (why not a naive guard): the obvious guard "receiver with topics requires a route Session.SessionID==its SessionID" FALSE-POSITIVES on amqp10, whose receivers self-establish links on start independent of the plan (amqp10/session.go:271-272; plan feeds only Health). validate/ is config-only and cannot distinguish plan-driven (mqtt/amqp091) from self-establishing (amqp10) transports without adapter knowledge arch-lint forbids. Correct fix needs a transport capability (e.g. PlanDrivenSubscriptions) surfaced to the builder, OR move the check to the builder where transport factories/Capabilities() are available. Verified against source 2026-06-30.
- **Resolution (2026-07-02):** Fixed functionally (no validate-layer guard, so the amqp10 false-positive is avoided). `bridge/builder_complete.go`'s `RegisterSessionSender` loop now sets `sc.Plan = sessionPlanFor(b.cfg, bd.SessionID)`, so a session that previously received only a planless sender manager now also carries the plan needed to reconcile its plan-driven receiver subscriptions (harmless for amqp10/servicebus where the plan feeds only Health; necessary for mqtt/amqp091). **Scope note:** this closes the *planless-manager* shape. A distinct adjacent shape — a receiver whose `session_id` resolves to **no manager at all** — is out of scope and tracked below as `ADV-P4-FU1`.

### LOW (40)

#### `ADV-P4-FU1` — bridge/builder
Receiver whose `session_id` resolves to NO session manager (neither plan-driven nor planless) silently subscribes to nothing — the missing-manager sibling of `ADV-F1-P4`.

- **Files:** `bridge/builder_complete.go`, `bridge/specs.go`
- **Notes:** Surfaced by the adversarial review of the `ADV-F1-P4` fix (2026-07-02). P4 closed the *planless-manager* case (a session that gets a planless sender manager now also carries a reconcilable plan). This item is the distinct case where `receiver.SessionID` maps to no manager whatsoever, so nothing ever reconciles its subscriptions and the receiver is silently inert. A naive validate-layer guard false-positives on amqp10 (self-establishes links, plan-independent), same reason `ADV-F1-P4` was not fixed in `validate/`. Correct fix: surface a transport `PlanDrivenSubscriptions` capability to the builder so it can require a session manager only for plan-driven transports (mqtt/amqp091) and skip the check for self-establishing ones (amqp10). Deferred: needs a new ports capability + builder threading, out of this branch's scope.

#### `OTEL-N3` — adapters/otel
Outbox-path causality gap: store-and-forward never injects the bridge hop at drain; downstream parents on upstream span (traced) or starts disconnected trace (untraced).

- **Files:** `runtime/route/dispatch.go`
- **Notes:** Strict improvement over pre-K1 (bridge span now joins upstream trace via Extract) and documented in dispatch.go. Full fix = persist+re-inject at drain time. Tracked.

#### `OTEL-N4` — adapters/otel/metrics
Metrics limit hot-path cliff: once cache full, every NEW name misses RLock fast path, takes Lock, rejected, calls onError on every emit → serialize + flood under dynamic-name misuse.

- **Files:** `adapters/otel/metrics/acl_client.go`
- **Notes:** K9 correctly trades OOM for bounded-but-noisy failure. Only bites operator misuse (runtime emits static set). Consider once-per-name/rate-limited rejection reporting.

#### `AMQP091-N1` — amqp091
setBlocked stale {Active:true} from a dropped-connection watcher can pin a healthy conn to Degraded+ErrBrokerBusy (no epoch guard)

- **Files:** `adapters/amqp/transport/amqp091/session.go`, `acl_client.go`
- **Notes:** NICE-TO-HAVE: pass a generation token / conn to setBlocked and ignore writes from a non-current connection.

#### `AMQP091-N2` — amqp091
map-path ReceiverConfigFromOptions lets explicit prefetch_count:0 mean unlimited, contradicting typed path 0->defaultPrefetchCount

- **Files:** `adapters/amqp/transport/amqp091/config.go`
- **Notes:** NICE-TO-HAVE: low-level/manual users only (managed uses typed path); latent backpressure footgun.

#### `G-N1` — amqp10
amqp10 egress is a 3rd divergent policy (IsReservedHeader && !IsBridgeToBridgeHeader) vs StripInternalOnlyHeaders / strip-all

- **Files:** `adapters/amqp/transport/amqp10/acl_outbound.go`
- **Notes:** DECIDED: keep amqp10 fail-closed predicate (stronger; reviewer agrees more-correct). Unify only if a shared StripNonPropagatedReservedHeaders lands across ALL transports (cross-cutting, deferred).

#### `G-N2` — amqp10
F2 unhonored delayed-retry logged Debug only; ops-invisible retry-spacing loss

- **Files:** `adapters/amqp/transport/amqp10/acl_delivery.go`, `metrics.go`
- **Notes:** NICE-TO-HAVE: add MetricAMQP10DelayedRetryUnhonored counter + once-per-link Warn.

#### `G-N3` — amqp10
Health over-reports active when plan.Subscriptions > registered receivers during startup

- **Files:** `adapters/amqp/transport/amqp10/session.go`
- **Notes:** NICE-TO-HAVE (partly pre-existing): clamp active to link-derived counts.

#### `ASB-D1` — asb
ASB docs: broker-side MaxDeliveryCount auto-DLQ is a 2nd dead-letter sink; queue delayed-retry reuses MessageID (dedup-loss caveat); RAD+no-DLQ at-most-once drop

- **Files:** `adapters/azure/transport/servicebus/doc.go`
- **Notes:** DOC-DEFERRED (Wave 5). asb-adversarial approach-challenges: doc "exactly once in one place" overstated; subscription abandon-not-schedule merits ADR.

#### `ASB-N1` — asb
Pre-existing time.Sleep in delivery_autoextend_retry_test.go / delivery_test.go (fake-clock sync)

- **Files:** `adapters/azure/transport/servicebus/delivery_autoextend_retry_test.go`, `delivery_test.go`
- **Notes:** Reviewer scoped OUT (pre-existing, allowlisted). Track for a determinism sweep: signal-channel handshake instead of sleep.

#### `J-N1` — aws-stores
outbox keyCache slow leak: claimed-but-never-completed records evicted only on terminal Complete → unbounded in the limit under lease churn (bounded ~0 healthy)

- **Files:** `adapters/aws/store/dynamodboutbox/acl_store.go`
- **Notes:** aws-adv N1. LRU/size cap or evict-on-condition-failure.

#### `J-N3` — aws-stores
Complete returns ErrNotFound after GSI-retry exhaustion → re-delivery instead of Complete-retry on genuine GSI lag (at-least-once holds)

- **Files:** `adapters/aws/store/dynamodboutbox/acl_store.go`
- **Notes:** aws-adv N3. retryable/timeout error so caller retries Complete.

#### `J-N6` — aws-stores
cold-partition seed seed==0 does not persist a fence row → repeated bounded scan each Claim until first claim raises fence (self-heals, safe)

- **Files:** `adapters/aws/store/dynamodboutbox/acl_store.go`
- **Notes:** aws-adv N6. raiseFence raise-only so seed race safe.

#### `J-N7` — aws-stores
integration gap: J5/J2/J3/J6 unit-only vs fakes; Docker/LocalStack concurrent claimers + cold-seed race would close residual

- **Files:** `adapters/aws/store/**`
- **Notes:** aws-adv N7. Deferred to make check-all.

#### `C3-FU5` — cmd
Pre-existing (not from C3): both selects in run() read supDone. If the supervisor self-exits first, the first select consumes supDone, so the second select blocks until the full ShutdownTimeout elapses before returning. Bounded/benign (slow-but-correct shutdown). The C3 terminal path is unaffected (first select consumes terminalCh, not supDone).

- **Files:** `cmd/gobridge/main.go`
- **Notes:** Reviewer residual NICE-TO-HAVE, explicitly pre-existing + out of C3 scope. Fix: track whether supDone was already consumed, or use a bool/nil-channel guard on the second read.

#### `A8-R2-validate-stale-msg` — config/validate.go
validate.go:199-205 stale_claim_duration warning propagates the imprecise rationale ("without reducing duplicate sends", "closer to step_down_grace + 15s"). Real constraint: stale_claim must exceed the worst-case drain-batch timeout (same-owner stranded-claim recovery, DynamoDB-only); grace+15s is only a rule of thumb. Refine the warning message to state the drain-ceiling constraint.

- **Files:** `config/validate.go:199-205`
- **Notes:** From a8-resilience review. Pre-existing code, not added by A8; left untouched to keep A8 doc-scoped. A8 docs now state the correct rationale and remain consistent with the grace+15s rule-of-thumb the validator nudges toward.

#### `D2-FU1` — docs
Doc: visibility/lock timeout + auto-extend validator interaction + migration note

- **Files:** `docs/transports/*`
- **Notes:** Docs sweep: document validator skips SendTimeout<window/2 when auto_extend on; only auto_extend:false + short window + long send is rejected (correct). ASB LockDuration is client-side mirror of broker value; operators should mirror broker lock. Also (reviewer NTH): document that ASB lock_duration must match the broker entity LockDuration, else auto_extend:false + declared-short lock_duration produces a fail-fast validator rejection on the declared value.

#### `DOC-DEFERRED-S21` — docs
scenario 21 transform.New uses non-existent API (name arg + WithTransformFunc + domain.Envelope + env.Headers map-write)

- **Files:** `docs/scenarios/21-amqp-cross-protocol.md:249`
- **Notes:** Not a mechanical single-return fix: reflects an old/imagined transform API that never existed in current code (New(cfg)(*Processor,error), no name, no options, mapping-based). Setting a CONSTANT header (partner-version=2) may not be expressible via transform mappings -> needs capability reconciliation in Wave 5 cross-plugin docs sweep. Do NOT botch mechanically.

#### `J-N4` — docs
CloudWatch 256-byte dim prefix collision → two values sharing prefix collapse to one series (inherent to CW limit)

- **Files:** `adapters/aws/metrics/cloudwatch/acl_batcher.go`
- **Notes:** aws-adv N4. Doc caution.

#### `HTTP-N2` — http
SSE Send streams bridge-to-bridge x-bridge.* (tenant-id/forwarded-from/correlation/trace) to possibly-external subscribers

- **Files:** `adapters/http/transport/sender_sse.go`, `doc.go`
- **Notes:** H2 scoped to internal-only strip by design; DOC: if any SSE endpoint is public, operators must strip full x-bridge.* at the edge (or add per-destination egress ACL).

#### `HTTP-N3` — http
min-16 api_key floor is a silent breaking change for <16-char inline keys; no warning against reusing api_key as forward token

- **Files:** `adapters/http/transport/config.go`, `doc.go`
- **Notes:** Release-note the floor; warn that reusing api_key as ForwardToken reopens H1 spoofing.

#### `A6-R1` — httpapi
Expose running config_version on GET /topology for continuous fleet monitoring

- **Files:** `httpapi/monitor.go`, `spec/httpapi/http-api.yaml`, `spec/httpapi/components.yaml`
- **Notes:** A6 adversarial NICE-TO-HAVE N1. handleTopology (monitor.go ~145-165) returns instance_id/running/routes; s.cfg.ConfigProvider() is already wired and reachable, so the running config version could be surfaced as a response field for scrapeable fleet-convergence monitoring (no continuous metric/HTTP field exists today). Deferred from A6 because it is a contract change: requires updating the OpenAPI spec (spec/httpapi/http-api.yaml + components.yaml) and would pull in contract-reviewer scope — out of proportion for a LOW NICE-TO-HAVE during a doc/observability fix. A6 instead points operators at Supervisor.Config().Version (the real programmatic surface). Pick up as a contract-aware change in the Wave 4 httpapi unit.

#### `B9-FU-STRICTJSON` — httpapi
Optional: strict JSON body decode (reject trailing garbage / multi-value) shared across create/patch/inject handlers

- **Files:** `httpapi/admin_config.go`, `httpapi/admin.go`
- **Notes:** httpapi-adversarial NICE-TO-HAVE. B9 finding (malformed->silent default txn) is CLOSED. Residual: {"ttl":"90s"}JUNK->201 honors leading value + ignores trailing (NOT default, milder); null->201 default (well-formed, defensible); multi-value->first wins. Same one-value Decode pattern in handleInject(admin.go:149) + handleConfigTxnPatch(admin_config.go:127). Proper fix = shared decodeStrictJSON (Decode + assert dec.Decode(&struct{}{})==io.EOF) across all 3. YAGNI now: no data-loss/security impact, finding closed.

#### `C3-FU3` — mqtt
Make Paho session reusable (resubscribe on re-Start) for true in-process lease re-acquire, eliminating the process-restart cost for exclusive MQTT.

- **Files:** `adapters/mqtt/transport/paho/acl_session.go`
- **Notes:** Single-use is deliberate (client-ID uniqueness). Reusable would need careful clean-session/clientID handling. Follow-up, not required for correctness.

#### `MQTT-ADR1` — mqtt
ADR: MQTT in-flight QoS1/2 settlement on session step-down (drop-and-ack vs disconnect-first-redeliver vs EnableManualAcknowledgment+post-emit-ack)

- **Files:** `adapters/mqtt/transport/paho/acl_router.go`, `delivery.go`
- **Notes:** Reverted stop-gate => current model = forward-in-flight + broker-redelivers-un-acked-on-socket-teardown. True graceful drain is manager-deferred (C7 + manual-ack future). Ratify as ADR.

#### `MQTT-N1` — mqtt
Non-string bridge-to-bridge header values silently skipped on egress (v.(string) else continue), no counter

- **Files:** `adapters/mqtt/transport/paho/acl_headers.go`
- **Notes:** Pre-existing, consistent with sibling transports; out of strict scope. A non-string idempotency-key/tenant-id would drop without signal.

#### `L8-FU1` — processors/circuitbreaker
Pre-existing (NOT introduced by L1/L8/L11): fetch-breaker-then-use-outside-p.mu in Process — a concurrent evictOldest can drop the breaker another goroutine is mid-flight on, transiently yielding two breakers per key and dropping the orphan state update. Manifests only under cache-full high-cardinality churn (the regime L8 now surfaces via Stats().OpenEvictions). Proper fix (refcount or hold-lock-across-call) is non-trivial and risks serializing throughput — defer as its own unit.

- **Files:** `processors/circuitbreaker/processor.go`
- **Notes:** From cb-adversarial NICE-4.

#### `OTEL-N5` — runtime
Metrics Flush+Close share one halved shutdownTimeout/2 budget; slow Flush can push Close(provider.Shutdown) to deadline → spurious ctx error appended to Stop result (provider still shuts down).

- **Files:** `runtime/bridge.go`
- **Notes:** Harmless (Flush != shutdown, comment accurate). Tracer gets its own full closeTimeout. Give Close its own budget for clean shutdown errors.

#### `OTEL-N6` — runtime
Straggler drainer after close when Stop ctx pre-cancelled: select skips wg.Wait, proceeds to close metrics/tracer while drainer may emit.

- **Files:** `runtime/bridge.go`
- **Notes:** Benign — OTel counter/span calls on shut-down provider are no-ops (SDK concurrency-safe). Note only.

#### `A4-R1-replaycount-decouple` — runtime/outbox + domain/persistence + dynamo
Transient retries still increment replay_count, so an outage longer than the poison budget DLQs good messages (memory/SQLite ~6x sooner than DynamoDB). Root-cause fix: bound delivery attempts by record AGE not claim count. Larger change across aggregate, snapshot, Dynamo store. Marked with ponytail comment in loop.go Run.

- **Files:** `runtime/outbox/loop.go`, `domain/persistence/outbox.go`
- **Notes:** Deferred per adversarial reviewer; 5s floor is the ship-gate mitigation.

#### `A4-R2-other-releaseerr-ordering` — runtime/outbox/retry.go
Rare degraded path: if store Release returns a non-stale store error (e.g. SQLite I/O error), processRecord returns nil -> head counted as success + group continues -> later same-key record may overtake the stranded head. Requires a store write error precisely on Release. Reviewer-blessed fallback; no data loss (head recovers via version/stale reclaim).

- **Files:** `runtime/outbox/retry.go`
- **Notes:** Documented residual; fixing fully needs a second signal + conditional releaseRemainder, over-engineering a rare store-failure path.

#### `E5-FU2` — runtime/route
receiveCount hardcodes 3 transport header strings that mirror unexported adapter constants in separate modules with no compile/test pin; an adapter rename silently disables max_replay for that transport with zero failing tests.

- **Files:** `runtime/route/dispatch.go:330`, `335`, `340`
- **Notes:** Discovered by E5 adversarial review. Runtime cannot import adapter packages (layer rule) and keys are unexported (servicebus/headers.go:20, amqp10/headers.go:21, sqs inline acl_inbound.go:186+193). Coupling IS documented (dispatch.go:324-326 comment) and an adapter rename breaks the adapter own header test (e.g. sqs headers_test.go:68) alerting the dev, so not fully silent. Fix options: cross-module contract test, or move the 3 keys into a shared domain/messaging header-name registry both layers import (reviewer ADR, borderline/reversible). Defer; values stable + documented.

#### `E5-FU3` — runtime/route
headerInt returns (0,false) for a present-but-unparseable count -> receiveCount treats it as first delivery (rc=0) -> on DirectHold a permanently-failing recoverable send retries unbounded with no signal.

- **Files:** `runtime/route/dispatch.go:385-389`
- **Notes:** Discovered by E5 adversarial review. Fail-open is defensible (do not DLQ a good message on a parse error) but the silent unbounded retry is unobservable. Adapters only stamp well-typed int/uint32 on the direct ingress path, so this needs header injection or a future adapter regression to hit. Minimal fix: when key present but uninterpretable, emit a debug log/metric. uint32-max->int (64-bit) and float64 truncation are NON-ISSUES per reviewer. Defer.

#### `C3-FU4` — runtime/session
Add a real-adapter test exercising step-down concurrent with in-flight delivery (Probe 5 NICE-TO-HAVE) — current coverage uses fakes + the single-use regression.

- **Files:** `runtime/session`
- **Notes:** Adversarial Probe 5: no real-adapter step-down-concurrent-with-delivery test. Fakes + singleUseSession cover the manager logic; an integration-tagged test would harden it.

#### `A8-R1-leasettl-margin` — runtime/session/config.go
Optional non-zero margin so RenewInterval*MaxRenewFails < LeaseTTL (e.g. LeaseTTL 48-50s) or a per-renew timeout < RenewInterval, making the "tolerates 2 transient renew failures then recovers on the 3rd" claim literally true and lifting the final renewal off the expiry boundary. Resilience-auditor classed it NICE-TO-HAVE; equality is acceptable given DefaultConfig parity (120*3=360).

- **Files:** `runtime/session/config.go:118-126 (HAConfig) and :66-80 (DefaultConfig)`
- **Notes:** From a8-resilience review. MUST apply to BOTH HAConfig and DefaultConfig (identical zero-margin structure). No safety impact (lease store + version fencing guarantee single-commit); availability/spurious-failover only.

#### `SQS-D1` — sqs
Scenario docs 02/03/05 should note address:<queue-name> now accepted (not only full URL); F8 batch_size direct-API-only note in scenario 02

- **Files:** `docs/scenarios/02-sqs-to-sqs.md`, `docs/scenarios/03-mqtt-to-sqs.md`, `docs/scenarios/05-durable-shared-outbox.md`
- **Notes:** DOC-DEFERRED to Wave 5 docs sweep.

#### `SQS-N1` — sqs
Attr size cap counts header bytes only, ignores MessageBody (256KiB shared budget)

- **Files:** `adapters/aws/transport/sqs/acl_outbound.go`
- **Notes:** NICE-TO-HAVE from sqs-adversarial: thread len(Payload) into starting budget OR scope comment to header-bytes-only. Non-loss (MapError surfaces SQS oversize).

#### `SQS-N2` — sqs
attributeValue has no bool case; bool headers skipped uncounted (invisible)

- **Files:** `adapters/aws/transport/sqs/acl_outbound.go`
- **Notes:** NICE-TO-HAVE from sqs-adversarial: map bool->String or count in dropped metric so the loss is visible.

#### `D2-FU2` — sqs+asb
Bounds validation for SQS visibility_timeout / ASB lock_duration vs broker limits

- **Files:** `adapters/aws/transport/sqs/config_plugin.go`, `adapters/azure/transport/servicebus/config_plugin.go`
- **Notes:** ASB allows 5s..5min lock; Validate() does not clamp/check. Distinct from D2. Reviewer NICE-TO-HAVE #2.

#### `ADV-F1-P2` — validate
No validation that a receiver/session pair share a transport; mixed-transport session is a silent misconfig

- **Files:** `validate/blueprint_graph.go`
- **Notes:** sessionPlanFor unions receivers by SessionID regardless of Transport. Nothing guarantees all receivers on a session share the session transport (blueprint_graph.go:66-70 checks only FK existence). Not a regression (a mixed-transport session is already broken independent of the plan; amqp091 configFromPlan returns ok=false on foreign config -> default direct, no panic). Clean transport-neutral guard available: in the receiver collectIDs closure compare r.Transport against sessionsByID[r.SessionID].Transport when both non-empty. No false-positive risk. Low value (obvious misconfig).

### NICE (4)

#### `C7-N3` — adapters/mqtt/transport/paho
startCtx dead state: write-only in production after C7 (assigned acl_session.go:272, read only by tests). Remove field + bug_reconcile_before_start_test.go/BUG-A machinery, or document retention. Refresh stale reconcileMu doc (session.go:37-40).

- **Files:** `adapters/mqtt/transport/paho/session.go`, `acl_session.go`
- **Notes:** From c7-adversarial NICE-TO-HAVE #3. Cleanup caused by the fix; deferred to avoid destabilizing a verified MEDIUM fix - needs careful source read of the ordering invariant the BUG-A test guards.

#### `C7-N4` — adapters/mqtt/transport/paho
Probe-4 TOCTOU defense-in-depth: handleConnectionUp does not take reconcileMu, so an in-flight prior-connect reconcile could rewrite activeSubs after reset (narrow ephemeral-session window). Take reconcileMu in handleConnectionUp or re-validate post-Subscribe.

- **Files:** `adapters/mqtt/transport/paho/session_lifecycle.go`
- **Notes:** From c7-adversarial NICE-TO-HAVE #4. LOW/theoretical: persistent/exclusive sessions moot; ephemeral window yields redundant no-op or Subscribe-error->terminal (no silent loss). Lock-ordering change - defer to avoid deadlock risk on a verified fix.

#### `C7-N1` — adapters/mqtt/transport/paho + runtime
Transient vs permanent reconcile failure: a momentary reconnect-resubscribe blip terminates the whole bridge (manager.go:238 -> bridge.go:375). Consider bounded in-manager retry for non-ACL errors before going terminal.

- **Files:** `runtime/manager.go`, `runtime/bridge.go`
- **Notes:** From c7-adversarial NICE-TO-HAVE #1. Conscious trade-off: correct as-is (terminal+alarm right for permanent ACL failure).

#### `C7-N2` — runtime
Exclusive-path conflation: reconcile errors relabeled lease.lost (+MetricLeaseTransfers, LeaseStateLost) at manager_lease.go:114-123. Emit distinct reconcile-failure signal for accurate observability.

- **Files:** `runtime/manager_lease.go`
- **Notes:** From c7-adversarial NICE-TO-HAVE #2.

