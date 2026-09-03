# GoBridge on Kubernetes

The maintained profile for running GoBridge off AWS: a `Dockerfile` that builds
the reference composition root (`cmd/gobridge`) and one manifest,
`gobridge.yaml`, that runs it as a StatefulSet. It is exercised end to end on
every integration run — image build, init container, probes, traffic, a
ConfigMap reload, SIGTERM drain and restart — by `TestKubernetesProfile` in
`tests/integration` (`make test-integration`).

## What it is, and what the AWS image is

| | Shipped AWS image | Kubernetes profile |
|---|---|---|
| Binary | `gobridge-filebased` (`deployment/aws-filebased-config`) | `gobridge` (`cmd/gobridge`) |
| Image | `ghcr.io/mariotoffia/gobridge`, published **by digest** with every stable `cmd/gobridge/vX.Y.Z` release; `latest` guarded | built from source with this `Dockerfile`, pushed to **your** registry, pinned by digest |
| Transports | MQTT, AWS SQS, HTTP | MQTT |
| Stores | memory, SQLite, DynamoDB | memory, SQLite |
| Secrets | SSM Parameter Store (`admin_api_key_param` is mandatory) | a Secret through `GOBRIDGE_ADMIN_API_KEY` / `GOBRIDGE_MONITOR_API_KEY`; `file://` credentials from a mounted Secret |
| Config delivery | EFS / bootstrap JSON | ConfigMap volume, hot-reloaded |
| HTTP API | addresses and keys from the bootstrap, TLS at the ALB | the config's `http:` block, optional in-process TLS |
| Clustering | DynamoDB HA facade | single replica per StatefulSet (no distributed lease store in this adapter set) |

The AWS image cannot run here: it resolves its secrets through SSM and builds a
DynamoDB client unconditionally. For a transport this profile does not bundle
(SQS, Azure Service Bus, AMQP), build your own composition root from
`cmd/gobridge/main.go` (register the decoders and the supervisor factories —
[PLUGIN.md](../../PLUGIN.md)) and build it with the same `Dockerfile`:
`--build-arg BINARY_MODULE=cmd/mybridge`. The manifest does not change.

## Build and push

From the repository root — the module resolves the rest of GoBridge through
relative `replace` directives:

```bash
docker build -f deployment/kubernetes/Dockerfile -t registry.example.com/gobridge:0.3.6 .
docker push registry.example.com/gobridge:0.3.6
docker buildx imagetools inspect registry.example.com/gobridge:0.3.6 --format '{{.Manifest.Digest}}'
```

Put the printed digest into both `image:` fields of `gobridge.yaml`
(`registry.example.com/gobridge@sha256:…`). A pod spec that names a tag can
pull a different image on its next restart; a digest cannot.

## Deploy

1. The manifest expects the broker at `mqtt-broker:1883` in the same
   namespace. Point `broker_url` in the ConfigMap at yours.
2. Replace the Secret's `admin-api-key` (16 characters minimum). Add
   `monitor-api-key` and a `GOBRIDGE_MONITOR_API_KEY` env entry if the monitor
   API should take its own key.
3. `kubectl apply -f deployment/kubernetes/gobridge.yaml`

The StatefulSet gives the pod a stable hostname (`gobridge-0`), which the
session's `client_id_suffix: hostname` turns into a stable broker client id —
the broker's persistent session, with its queued QoS 1/2 messages, is keyed to
it. `assert_stable_client_identity: true` is the operator vouching for that;
the same configuration on a Deployment would mint a new client id on every
rollout and strand the previous session's queue (see the
[deployment identity table](../../docs/transports/mqtt.md#deployment-identity)).

### The init container

A persistent session with subscriptions keeps the exact filters it installed
on the broker in `stores.managed_subscriptions` and will not connect until
that store holds a baseline row for its identity — a missing row is "history
unknown", not "no history" ([ADR 0003](../../docs/adr/0003-mqtt-persistent-session-hygiene.md)).
The `seed-managed-subscriptions` init container writes that row with
`-seed-managed-subscriptions mqtt-conn`, the attestation that the client id is
new. It is idempotent, so it runs on every pod start and never touches an
established baseline. If the client id already has subscriptions on the
broker, list them instead:

```yaml
args: ["-config", "/etc/gobridge/bridge.yaml", "-seed-managed-subscriptions", "mqtt-conn=sensors/#"]
```

Never attest an empty baseline for an identity whose subscriptions are merely
unknown — [durable sessions](../../docs/transports/mqtt-durable-sessions.md).

## Verify

```bash
kubectl get pod gobridge-0                      # Running, READY 1/1
kubectl port-forward pod/gobridge-0 8081:8081 8080:8080 &
curl -s localhost:8081/api/v1/monitor/live       # {"status":"alive"}
curl -s 'localhost:8081/api/v1/monitor/ready?level=subscribed'
curl -s -H "X-API-Key: $ADMIN_KEY" localhost:8080/api/v1/admin/bridge
```

Readiness gates on `subscribed`: the broker has acknowledged every
subscription, so the pod misses nothing once it is in the Service. The admin
API stays reachable through the headless `gobridge-admin` Service while the pod
is not ready (paused, or waiting for its broker) — that is when an operator
needs it. Probe levels and the shutdown sequence:
[Health Checks and Graceful Shutdown](../../docs/health-and-shutdown.md).

## Reload

Edit the ConfigMap and apply it. Kubernetes swaps the volume's `..data`
symlink atomically; the watcher (`config_watch.mode: notify`) sees the swap,
and its 30-second hash resync covers a missed event. The new routes are live
without a restart. Two rules keep that true:

- mount the ConfigMap as a **volume**, never a `subPath` — a `subPath` copy is
  not updated;
- any other writer of `bridge.yaml` must write atomically (render, then
  rename) — [external writers](../../docs/runbooks/external-config-atomic-writes.md).

Changing the `http:` addresses or the durable session's broker identity takes
a restart (`kubectl rollout restart statefulset/gobridge`); the reload accepts
the file and `/deephealth` reports `restart_required`.

## Restart and upgrade

`kubectl rollout restart statefulset/gobridge` drains within
`bridge.shutdown_timeout` and exits 0; the grace in the manifest (120 s) is
above twice that budget, which is `cmd/gobridge`'s worst case. The new pod
resumes the same broker session from the state volume. To upgrade, build and
push a new image, put its digest into both `image:` fields, and apply.

## Credentials

Broker passwords and TLS material come from a Secret mounted read-only and
referenced by `credentials_uri: file://…`, rotated live on Secret update —
[Scenario 22](../../docs/scenarios/22-k8s-secret-mount-credentials.md). Add
`-credentials-dir /etc/gobridge/creds` to the container args and mount the
Secret there.
