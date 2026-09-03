# Container and Orchestrator Deployment

Pinning images by digest, wiring probes into an orchestrator, building your own
image for non-AWS Docker or Kubernetes, and mounting configuration from a
ConfigMap. Split out of [Deployment Guide](deployment-guide.md); these sections
were nested under health checks, which is not where a reader looks for them.

## Pin Images by Digest

Every stable `cmd/gobridge/vX.Y.Z` release pushes `ghcr.io/mariotoffia/gobridge`
**by digest** and attaches the verified digest to that command release as
`gobridge-image-digest.txt`; no `vX.Y.Z` container tag exists. One mutable tag
exists — `latest` — promoted after the vulnerability scan from the exact
released digest, and only when the release is the highest stable command
release, so a re-run of an older release never moves it
([RELEASE.md](../RELEASE.md#image-publication)).

`latest` is for an interactive `docker pull`. A task definition, pod spec, CDK
construct or `Dockerfile` **must** reference the digest
(`ghcr.io/mariotoffia/gobridge@sha256:...`): a moving tag makes a rebuild
non-reproducible and can pull an unexpected image on the next deploy. The same
rule applies to the `Dockerfile` base images. Take the digest from the release
asset; to see what `latest` currently resolves to:

```bash
docker buildx imagetools inspect ghcr.io/mariotoffia/gobridge:latest \
  --format '{{.Manifest.Digest}}'
```

The [image upgrade/rollback runbook](runbooks/upgrade-rollback-and-sqlite-durability.md#pin-images-by-digest)
covers upgrading and rolling back between digests.

## Container Orchestrator Integration

**ECS Task Definition:**

```json
{
  "healthCheck": {
    "command": ["CMD", "/usr/local/bin/gobridge-filebased", "-healthcheck"],
    "interval": 10,
    "timeout": 5,
    "retries": 3,
    "startPeriod": 60
  },
  "stopTimeout": 60
}
```

The image ships no shell, `curl`, or `wget`, so the health check invokes the
binary's `-healthcheck` flag (which probes the local monitor `/live` endpoint).
The CDK facades set `stopTimeout` to 60s; size it above `shutdown_timeout`.

**Kubernetes Pod Spec:**

```yaml
livenessProbe:
  httpGet:
    path: /api/v1/monitor/live
    port: 8081
  initialDelaySeconds: 5
  periodSeconds: 10
readinessProbe:
  httpGet:
    path: /api/v1/monitor/ready?level=subscribed
    port: 8081
  initialDelaySeconds: 10
  periodSeconds: 5
terminationGracePeriodSeconds: 60
```

**Admin API reachability.** The readiness probe deliberately fails a paused or
not-yet-connected pod, which removes it from the Service's ready endpoints. The
admin API (port 8080) then becomes unreachable through a normal `ClusterIP`
Service exactly when an operator needs it to start or diagnose the bridge. Expose
the admin port through a Service with `publishNotReadyAddresses: true` (or a
headless Service, `clusterIP: None`) so a not-ready pod's admin API stays
routable; `kubectl port-forward` reaches it directly regardless. The liveness
probe stays 200 through a clean `POST /bridge/stop`, so the pod is not restarted
while paused.

**Readiness levels.** The readiness probe accepts a `?level=` query
parameter that controls how strict the gate is. Bare
`/api/v1/monitor/ready` (no level) reports ready as soon as the runtime is
started and healthy -- *before* transport sessions connect or subscriptions
are acknowledged -- so a pod can be added to the Service endpoints while it
would still miss messages. Gate production traffic on a transport-level
check instead. Supported levels, least to most strict:

- `live` -- process is up and serving HTTP. Use for liveness, not readiness.
- `running` -- runtime started and healthy (the level bare `/ready`
  approximates).
- `connected` -- every session is currently connected to its broker; a
  per-session reconnect drops below this level (503) until the session
  reconnects.
- `subscribed` -- every subscription has been acknowledged by the broker,
  so the bridge will not miss messages. **Recommended for readiness
  gating** and used in the example above.
- `full` -- every planned receiver handler is registered, every unique desired
  subscription filter is active at or above its requested QoS, no MQTT publish
  remains buffered waiting for a handler, and every route is ready to dispatch.
  This is the strictest gate, suitable as a pre-traffic check on initial rollout.

The probe returns `200` once the runtime has reached the requested level and
`503` otherwise, so Kubernetes holds the pod out of rotation until it is
genuinely ready to carry traffic. An unknown level returns `400`.

Set the orchestrator's stop/termination timeout higher than `shutdown_timeout`
to give GoBridge enough time to drain before the orchestrator sends `SIGKILL`.

## Kubernetes and Non-AWS Docker

The published image (`ghcr.io/mariotoffia/gobridge`, see
[Pin Images by Digest](#pin-images-by-digest)) is the **AWS file-based
profile**: it reads its bootstrap from env/SSM and builds a DynamoDB client
unconditionally. It is **not** a general off-AWS image — running it outside AWS
without SSM and the expected bootstrap will not work.

Off AWS, run the maintained **[Kubernetes profile](../deployment/kubernetes/README.md)**
instead: a Dockerfile that builds the reference binary (`cmd/gobridge` — MQTT
transport, memory/SQLite stores, `file://` credentials, in-process HTTP API
with optional TLS) and one manifest that runs it as a StatefulSet with a
ConfigMap-mounted `bridge.yaml`, the admin key from a Secret, a persistent
volume for the SQLite state, an init container that seeds the durable MQTT
session's baseline, and the liveness/readiness probes below. The profile is
built from source and pushed to your own registry; pin it by digest like any
other image. It is exercised on every integration run through probes,
traffic, a ConfigMap reload, SIGTERM drain and restart (`TestKubernetesProfile`
in `tests/integration`).

For transports the profile does not bundle (SQS, Azure Service Bus, AMQP), build
your own composition root from `cmd/gobridge/main.go` — register the decoders
(`Register(reg)`) and the supervisor factories for the adapters you use (see
[Reference Binary and Composition Root](deployment-guide.md#reference-binary-and-composition-root)
and [PLUGIN.md](../PLUGIN.md)) — and point the profile's Dockerfile at it. The
manifest needs no change: the reference binary's flags, ports and probes are
the ones your root inherits.

## Kubernetes ConfigMap Config

When the bridge config comes from a ConfigMap, mount the **volume** (not a
`subPath`) and point `config_file_path` at the file inside it. Kubernetes
updates a ConfigMap volume by writing a new timestamped directory and swapping
the `..data` symlink atomically. The file watcher in `notify` mode watches the
mount **directory**, so it sees the symlink swap; a `subPath` mount is a copy
that Kubernetes **never** updates in place, so hot-reload will not fire. As a
backstop the watcher re-hashes on a resync ticker (30s default) even in notify
mode, which also covers network filesystems that drop inotify events.

**TLS.** How TLS terminates depends on which binary you run.

- **`cmd/gobridge` (the Kubernetes profile) and library embeddings** honor the
  config `http:` block: set `tls_cert_file` and `tls_key_file` (both, or
  neither) to serve the admin and monitor APIs over HTTPS in-process
  (`cmd/gobridge/main.go`). Renewed certificates hot-reload without a restart —
  the server reloads the pair when either file's modification time changes on
  the next TLS handshake. The API keys may come from the environment
  (`GOBRIDGE_ADMIN_API_KEY`, `GOBRIDGE_MONITOR_API_KEY`) so the ConfigMap never
  carries them. See the [`http:` field reference](http-api.md#http-api-configuration).
- **The AWS file-based profile (the shipped image) does not honor the `http:`
  block.** It sources the admin/monitor listen addresses, CORS origins, and API
  keys from the bootstrap config (env/SSM) rather than `bridge.yaml`
  (`deployment/aws-filebased-config/lib/bootstrap/app.go`), and it sets
  no in-process TLS — TLS terminates at the ALB in front of the task. The
  `http:` block's `tls_cert_file` / `tls_key_file` (and its admin/monitor
  addresses and keys) are ignored on that image.
