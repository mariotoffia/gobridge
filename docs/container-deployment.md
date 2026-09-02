# Container and Orchestrator Deployment

Pinning images by digest, wiring probes into an orchestrator, building your own
image for non-AWS Docker or Kubernetes, and mounting configuration from a
ConfigMap. Split out of [Deployment Guide](deployment-guide.md); these sections
were nested under health checks, which is not where a reader looks for them.

## Pin Images by Digest

For reproducible builds, pin images by digest (`name@sha256:...`) rather than a
floating tag such as `:latest` — both the `Dockerfile` base/runtime images and
the GoBridge image referenced in task/pod specs. A moving tag makes a rebuild
non-reproducible and can pull an unexpected image on the next deploy. No image
tags are published yet: the release workflow pushes the GoBridge image **by
digest only** (never `ghcr.io/...:vX.Y.Z`) after the first `cmd/gobridge/vX.Y.Z`
command release is cut, recording the verified digest in
`gobridge-image-digest.txt`. The `v0.1.0` / `v0.2.0` / `v1.2.3` tags in these
examples are illustrative placeholders; at release, pin the concrete digest from
that asset (see [RELEASE.md](../RELEASE.md)). The
[image upgrade/rollback runbook](runbooks/upgrade-rollback-and-sqlite-durability.md#pin-images-by-digest)
shows how to resolve a tag to its digest.

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

## Non-AWS Docker / Kubernetes (build your own image)

The image (`ghcr.io/mariotoffia/gobridge`, published by digest at the first
command release) is the **AWS file-based
profile**: it reads its bootstrap from env/SSM and registers the AWS-oriented
composition root. It is **not** a general off-AWS image — running it outside AWS
without SSM and the expected bootstrap will not work. For a non-AWS Docker/K8s
deployment you ship **your own binary** built from a composition root that
registers only the transports, stores, and processors you use.

**1. Composition root.** Copy `cmd/gobridge/main.go` as the template and register
your adapters (the `Register(reg)` decoder calls plus the supervisor factories).
See [Reference Binary and Composition Root](deployment-guide.md#reference-binary-and-composition-root)
and [PLUGIN.md](../PLUGIN.md).

**2. Dockerfile skeleton** (multi-stage; the image ships no shell, so probe with
the binary or an HTTP probe):

```dockerfile
# Build stage
FROM golang:1.25 AS build
WORKDIR /src
COPY . .
# Point at YOUR composition root, not cmd/gobridge (the demo binary).
RUN CGO_ENABLED=0 go build -o /out/mybridge ./cmd/mybridge

# Runtime stage
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mybridge /usr/local/bin/mybridge
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mybridge", "-config", "/etc/gobridge/bridge.yaml"]
```

**3. Kubernetes manifests (template — requires your own image).** The snippets
below are a starting point; substitute your image and the config your bridge
actually references. The readiness probe uses `/api/v1/monitor/ready` (gate on a
transport-level `?level=` — see [Readiness levels](#container-orchestrator-integration)).

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: gobridge-config
data:
  bridge.yaml: |
    bridge:
      id: my-bridge
    # sessions / receivers / senders / routes ...
---
apiVersion: v1
kind: Secret
metadata:
  name: gobridge-secrets
type: Opaque
stringData:
  admin-api-key: change-me-to-a-real-secret-key
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gobridge
spec:
  replicas: 1
  selector:
    matchLabels: { app: gobridge }
  template:
    metadata:
      labels: { app: gobridge }
    spec:
      containers:
        - name: gobridge
          image: registry.example.com/mybridge@sha256:...  # your image, pinned by digest
          args: ["-config", "/etc/gobridge/bridge.yaml"]
          ports:
            - { name: admin, containerPort: 8080 }
            - { name: monitor, containerPort: 8081 }
          env:
            - name: GOBRIDGE_ADMIN_API_KEY
              valueFrom:
                secretKeyRef: { name: gobridge-secrets, key: admin-api-key }
          volumeMounts:
            - { name: config, mountPath: /etc/gobridge }
          livenessProbe:
            httpGet: { path: /api/v1/monitor/live, port: 8081 }
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /api/v1/monitor/ready?level=subscribed, port: 8081 }
            initialDelaySeconds: 10
            periodSeconds: 5
      terminationGracePeriodSeconds: 60
      volumes:
        - name: config
          configMap: { name: gobridge-config }
---
apiVersion: v1
kind: Service
metadata:
  name: gobridge
spec:
  selector: { app: gobridge }
  ports:
    - { name: monitor, port: 8081, targetPort: 8081 }
```

Expose the admin port (8080) through a Service with
`publishNotReadyAddresses: true` (or a headless Service) so a not-ready pod's
admin API stays reachable — see [Admin API reachability](#container-orchestrator-integration)
above. Mount the config as a **volume**, not a `subPath`, so hot-reload works
(see [Kubernetes ConfigMap Config](#kubernetes-configmap-config)).

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

- **`cmd/gobridge` and library embeddings** honor the config `http:` block: set
  `tls_cert_file` and `tls_key_file` (both, or neither) to serve the admin and
  monitor APIs over HTTPS in-process (`cmd/gobridge/main.go:233-234`). Renewed
  certificates hot-reload without a restart — the server reloads the pair when
  either file's modification time changes on the next TLS handshake. See the
  [`http:` field reference](http-api.md#http-api-configuration) for the fields.
- **The AWS file-based profile (the shipped image) does not honor the `http:`
  block.** It sources the admin/monitor listen addresses, CORS origins, and API
  keys from the bootstrap config (env/SSM) rather than `bridge.yaml`
  (`deployment/aws-filebased-config/lib/bootstrap/app.go:301-305`), and it sets
  no in-process TLS — TLS terminates at the ALB in front of the task. The
  `http:` block's `tls_cert_file` / `tls_key_file` (and its admin/monitor
  addresses and keys) are ignored on that image.
