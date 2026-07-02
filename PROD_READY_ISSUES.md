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
