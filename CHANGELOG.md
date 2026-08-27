# Changelog

All notable changes to GoBridge are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**One version for everything.** Every published module in this repository
carries the same version, so a single entry below describes the whole release —
there is no per-module changelog. See [RELEASE.md](RELEASE.md#one-version-for-everything).

## [Unreleased]

### Added

- The two CDK modules `deployment/aws-filebased-config/cdk` and
  `deployment/aws-filebased-config/infra` are now published, taking the train
  from 31 to 33 modules. The CDK scenarios under `docs/scenarios/cdk/` tell an
  external app to import the constructs and the `infra` types those constructs
  take as arguments, but neither module had ever been tagged, so none of the
  documented examples could resolve outside this repository.
  `deployment/aws-filebased-config/lib` stays internal — it is wiring for the
  shipped image, not a consumer API.

### Changed

- `cmd/gobridge` moved from layer 3 to layer 4. `cdk` requires the layer-2 store
  aggregates, so it lands on layer 3, and the final module must be alone on the
  highest layer for the strict all-module gate and image build to trigger once.
- `scripts/release/run.sh` derives the highest layer from the manifest instead of
  hardcoding `0 1 2 3`, so adding a module above the current top layer no longer
  needs an edit to the script.

### Fixed

- `TestAutoExtendStopsAfterMaxFailuresS15` failed under parallel load. The SQS
  auto-extend loop re-arms its ticker *after* the `ChangeMessageVisibility` call
  returns, so waiting on the recorded call did not prove the ticker carried its
  shortened retry deadline. Advancing the fake clock into that gap stepped past
  the stale deadline without firing, and the late `Reset` then scheduled from
  the new now — stranding the third tick. `clocktest.Fake` gained
  `TickerResets()`, the re-arm counterpart to `TickerCount()`, and the test now
  waits for the re-arm before advancing.

## [0.3.3] - 2026-07-27

Completes 0.3.2: same modules, plus the two release-pipeline fixes that
0.3.2's own train exposed. This is the first version whose train runs clean
end to end, including the container image association and `latest` promotion.

### Fixed

- `image-association` used a single variable both to re-resolve the tag being
  released and to locate the Release hosting the digest asset. Once Releases
  became root-only these are different commits — root is tagged first — so the
  job failed after the image had already built and passed its vulnerability
  scan. Split into `VERIFY_TAG` and `RELEASE_TAG`. In 0.3.2 this left the
  scanned image published to GHCR by digest but not recorded on the release,
  and `latest` unmoved.
- Waiting on a layer spawned one `gh run watch` per module, which for the
  26-module layer exhausted the GitHub API rate limit and failed the layer on
  `HTTP 403` even though every workflow had succeeded. A single `gh run list`
  per poll cycle now covers a whole layer, so polling cost is constant
  regardless of layer size.

## [0.3.2] - 2026-07-27

Re-release of the withdrawn 0.3.1. Same contents, plus the push fix below.

### Fixed

- Release trains pushed a layer's tags in one batch. GitHub does not create a
  workflow event for every ref in a bulk tag push — past roughly three tags the
  remainder silently get no run at all — so 26 layer-1 tags published without
  ever being verified. Tags are immutable, so they could not be re-triggered.
  Tags are now pushed one at a time; the concurrent part is the workflow wait,
  which is where the time actually goes.

## [0.3.1] - 2026-07-27 [WITHDRAWN]

**Do not use.** Only 27 of 31 modules were tagged, and 26 of those were never
verified by the release workflow. `cmd/gobridge`, `httpapi`,
`adapters/aws/store` and `adapters/native/store` have no 0.3.1 tag at all, so
the version is not a usable set. Superseded by 0.3.2, which contains
everything below.


Security patch, plus the release pipeline fixes that the 0.3.0 train exposed.

### Security

- `golang.org/x/net` raised to `v0.57.0` across every module.
  [CVE-2026-25681](https://github.com/advisories) (HIGH, arbitrary code
  execution in `golang.org/x/net/html`) affects `v0.52.0`, which 0.3.0 resolved
  transitively. The 0.3.0 container image was never published for exactly this
  reason: the release workflow's Trivy gate refused it. The Go modules were
  published, so anyone on 0.3.0 should move to 0.3.1.

### Fixed

- The release train raced the Go module proxy. A tag push starts the release
  workflow immediately, and its first act is to resolve from
  `proxy.golang.org` the module that push just created — before the proxy has
  fetched it. Both the published-module and internal-helper resolutions now
  wait on the observable state (module resolves, reports its `go.mod`, origin
  matches the tag commit) with time only as the failure budget. A wrong path,
  wrong version or mismatched origin still fails immediately; waiting cannot
  fix those.
- The release train could not be resumed. Tags are immutable, so a train that
  stopped part-way could only be continued, never restarted — re-running it
  aborted on `git tag` for an already-published module. Publishing now skips a
  tag that exists on the remote while still applying every gate to it.
- Five modules (`cmd/gobridge`, the CDK profile, `scripts/pluginsym`,
  `scripts/registrychk`, `tests/docsexamples`) could not `go mod tidy` outside
  the workspace: they reached `testutil/mqttlocal` through the paho adapter's
  tests but carried no `replace` for it. Every module in the repository now
  tidies standalone.

- A data race in `adapters/mqtt/transport/paho`: a test's ack counter was
  written from the router's grace-loop goroutine and read from the test body
  without synchronisation. Making it atomic exposed the real defect underneath
  — the assertion assumed the ack had already happened, when it can come from
  either `Reconcile`'s settle pass or the grace loop. It now waits on that
  state instead of assuming an ordering the router never promised.

### Changed

- One GitHub Release per version instead of one per module. All 31 modules
  still get tags, strict verification and proxy checks; only the root tag gets
  a human-facing Release page. The image digest asset attaches to it.
- Release trains publish each dependency layer concurrently instead of one
  module at a time. A layer means "these modules do not depend on each other",
  so waiting a full workflow round-trip between each of layer 1's 26 modules
  was pure queueing — about 80 minutes of it. Layers remain strictly
  sequential, because staging a layer runs `go mod tidy` against the previous
  layer's published versions. No gate changed: every tag still gets its own
  workflow, strict verification and proxy check.

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

[Unreleased]: https://github.com/mariotoffia/gobridge/compare/v0.3.3...HEAD
[0.3.3]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.3
[0.3.2]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.2
[0.3.1]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.1
[0.3.0]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.0
