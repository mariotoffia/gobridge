# Changelog

All notable changes to GoBridge are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**One version for everything.** Every published module in this repository
carries the same version, so a single entry below describes the whole release —
there is no per-module changelog. See [RELEASE.md](RELEASE.md#one-version-for-everything).

## [Unreleased]

## [0.3.0] - 2026-07-26

First release that can be installed from outside this repository. Earlier tags
(`v0.1.0`, `v0.2.0`) were root-only, had no nested module tags, and are not
consumable.

### Added

- **Transports**: MQTT v5 (shared sessions, QoS 0/1/2, wildcards, reconnect),
  AWS SQS (long polling, batch send, visibility extension, FIFO), Azure Service
  Bus (queues, topics/subscriptions, batch send, auto-extend lock), RabbitMQ /
  AMQP 0-9-1 (exchanges, bindings, publisher confirms, prefetch), AMQP 1.0
  (Artemis, Solace, Qpid), and HTTP (POST ingress, SSE egress).
- **Delivery guarantees**: `DirectHold` (send-then-ack) and `SharedOutbox`
  (persist-then-ack with a durable outbox drainer).
- **Stores**: lease, outbox, dead-letter, rollout and managed-subscription
  stores, with in-memory, SQLite and DynamoDB implementations.
- **Processor chain**: filter, transform, circuit breaker and tenant isolation.
- **Clustering**: lease-based exclusive ownership so multiple replicas process a
  stream exactly once, with automatic failover, plus coordinated configuration
  rollout across the cluster.
- **Dead-letter management**: poison messages are diverted rather than dropped
  or left to block the queue, and can be inspected and re-submitted.
- **Credentials**: URI-based resolution (`file://`, `pms://`) with scheme
  dispatch and caching.
- **HTTP APIs**: admin server for bridge lifecycle, route injection and DLQ
  management; monitor server for health probes and topology.
- **Observability**: OpenTelemetry metrics and tracing, CloudWatch metrics, and
  correlation-aware structured logging via `slog`.
- **Zero-dependency core**: the root module has no external dependencies; only
  the adapters you import pull anything in.
- 31 independently installable modules published under one shared version.

### Fixed

- The root module could not be released at all: its `bridge` rollout tests
  imported the `memorylease` and `memoryrollout` adapter modules, which the
  layer-0 root may not require. Both are now root-owned packages under
  `adapters/native/`, restoring the 31-module / 26-layer-1 set the release
  policy describes.
- `make test-long-running` and `make test-failover-gate` globbed
  `./tests/longrunning/...` from the repository root, which is a separate
  module. They resolved only through `go.work` and so never ran in CI. Both now
  use `go -C`.

### Changed

- Container test fixtures gate on protocol truth rather than on an open port. A
  published Docker port is bound by docker-proxy at container creation, so a TCP
  dial succeeds while the service is still starting or already dead. The Service
  Bus and Mosquitto fixtures now require a real message roundtrip before
  reporting ready.
- A fixture that fails to start is now a test failure wherever it could have
  started, instead of a skip. Previously any startup failure was swallowed by
  `t.Skipf`, which is how a permanently broken Service Bus emulator reported
  `ok` for its entire package. Skips remain only when Docker is absent, or for a
  declared prerequisite such as `LOCALSTACK_AUTH_TOKEN`.
- All container images used by tests are pinned by digest. Floating `:latest`
  tags meant CI could break on a day nobody changed anything.
- Fixture teardown drains containers (SIGTERM, wait for stop, remove, wait for
  removal) instead of `docker rm -f`, which killed brokers mid-flush and
  returned before Docker had finished.
- Images are fetched before `docker run` rather than through its implicit pull,
  which had to complete inside a 90-second timeout and therefore worked on a
  warm cache and failed on a cold one.
- `testutil/mqttlocal` is now its own module, so it can use a real MQTT client
  for its readiness probe without adding a dependency to the root module.
- The nightly scheduled CI run is paused; the full suite still runs on every
  push and pull request.

### Known limitations

- `cmd/gobridge` is a demonstration binary. It links only MQTT and the native
  stores and rejects configurations using anything else. Build a composition
  root for real deployments.
- The published container image is a release candidate. The production approval
  described in [RELEASE.md](RELEASE.md#image-publication) is a separate,
  credentialed step.
- The AWS SSM credential adapter and the CloudWatch metrics adapter have no
  integration coverage in CI: their tests depend on LocalStack, which requires a
  licence token that is not configured. Set `LOCALSTACK_AUTH_TOKEN` to run them.

[Unreleased]: https://github.com/mariotoffia/gobridge/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.0
