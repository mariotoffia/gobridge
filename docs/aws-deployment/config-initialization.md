# Initial configuration and missing-config lifecycle

## Overview

GoBridge can carry an initial logical configuration inside its binary. At
process startup, the control node may use that document to create a missing
configuration in the selected target: a file or a DynamoDB item. An existing
document always wins. Initialization is not a deployment-time overwrite policy.

For example, an image can contain routes for `orders-in` and `orders-out`.
Its first control process creates the target at version 1. Later image
deployments leave an operator-edited target unchanged.

The process starts its control plane with valid bootstrap settings and keeps
the data plane idle until a valid configuration can be activated. No
configuration seeder sidecar, init container, download script, or separate
configuration image is required.

Terms are defined in the [project glossary](../../UBIQUITOUS.md) and the
[profile glossary](../../deployment/aws-filebased-config/UBIQUITOUS.md).

## Control-plane startup

The reference command, `cmd/gobridge`, needs listener and authentication
settings; a blank build does not supply an authenticated admin service.
Set `-admin-addr` to start the control plane independently of the config
repository. Export `GOBRIDGE_ADMIN_API_KEY` with a key of at least 16 characters:

```bash
: "${GOBRIDGE_ADMIN_API_KEY:?Export a strong admin API key before starting}"
./cmd/gobridge/gobridge.out -config bridge.yaml \
  -admin-addr 127.0.0.1:8080 -monitor-addr 127.0.0.1:8081
```

This binds loopback listeners and can accept initial config while `bridge.yaml`
is absent. It does not create a synthetic data-plane runtime. An embedded
initial document may create the file first; otherwise an operator can use
the authenticated creation API described under [Operator creation](#operator-creation-and-rollout).

| Setting | Purpose |
|---|---|
| `-admin-addr` | Explicit process-owned admin address; enables repository-independent control startup. |
| `-monitor-addr` | Process-owned monitor address; defaults to `:8081`. |
| `-http-tls-cert`, `-http-tls-key` | Certificate and private-key files for process-owned Transport Layer Security (TLS). Supply both when terminating TLS in the process. |
| `GOBRIDGE_ADMIN_API_KEY` | Admin authentication key, supplied independently of the bridge document. |
| `GOBRIDGE_MONITOR_API_KEY` | Optional separate monitor key; otherwise the admin key is used. |

Without explicit `-admin-addr`, legacy boot-file `http:` settings remain
supported. That path must read the boot document before selecting its HTTP
settings; use explicit flags when that read must not block control startup.
`-start-empty` is accepted but deprecated. It does not enable unauthenticated
listeners or turn absence into an active empty runtime.

The AWS profile uses its existing `BootstrapConfig` listener settings and SSM
key references instead. Both commands handle `-initial-config-digest` before
bootstrap or network startup; inspecting embedded identity needs no API key.

## Build an initial document into the binary

Both `cmd/gobridge` and the AWS profile command,
`deployment/aws-filebased-config/lib/cmd/gobridge-filebased`, use `go:embed` to
read the fixed `initial-config.base64` file into `main.initialConfigBase64`.
The checked-in file is empty by default. Their entry points decode the value
and pass the logical document through the port-based initializer.
YAML and JSON are accepted; an empty payload means no initial document.
`parser.NewInlineSource(contents, registry)` exposes the decoded content through
`ports.Loader`; each load returns a fresh typed logical config.

Build the reference command at the repository root:

```bash
make build-gobridge \
  GOBRIDGE_TAGS=gobridge_mqtt,gobridge_native \
  INITIAL_CONFIG_FILE=path/to/initial.yaml
```

Build the AWS profile image with the root Dockerfile:

```bash
make docker-build INITIAL_CONFIG_FILE=config/initial.yaml

# Equivalent Docker input; the file must be inside the build context.
docker build --build-arg INITIAL_CONFIG_FILE=config/initial.yaml \
  -t gobridge:local .
```

The [Kubernetes Dockerfile](../../deployment/kubernetes/README.md#embed-an-initial-configuration)
builds the reference command instead. It accepts the same initial-config input,
plus `VERSION` and `GIT_SHA` metadata.

Select the plugin families the document needs. Embedding a document does not
link a missing transport, store, or processor. See
[binary composition](../../PLUGIN.md#binary-composition-build-tags).

Local Make and Docker builds run `scripts/buildconfig`, a Go standard-library
tool. It reads `INITIAL_CONFIG_FILE` and creates a Base64 payload plus
`overlay.json` in a temporary build directory. The Go overlay maps the command's
fixed `initial-config.base64` path to that generated payload during compilation;
original source files remain unchanged. Only file paths and small version flags
travel through command arguments. No payload is put in an environment variable.

Do not pass the payload through `GOFLAGS`, including flags loaded from `GOENV`.
Go exports `GOFLAGS` to child processes, so that approach still hits Linux
environment-size limits. The previous Go-environment-file helper is removed.
Go also rejects `@responsefile` syntax. File embedding replaces those paths;
normal config-size limits still apply. No additional software development kit
(SDK) or build tool beyond Go and Docker is required.

Reading the embedded source needs no S3 access or credential lookup. Writing
the selected target still requires access to that file or backend; runtime
transports and credential references retain their own access requirements.

### Inspect embedded identity

Both commands support `-initial-config-digest`. It prints the SHA-256 hash of
the decoded embedded document bytes and exits before runtime or network startup,
without printing the configuration. No runtime bootstrap or backend access is
needed to inspect the embedded identity:

```bash
./cmd/gobridge/gobridge.out -initial-config-digest
```

This is a byte-level build check, not a deployment-baseline comparison.
Changing whitespace in the embedded document changes this digest.

### CDK-built images

The AWS Cloud Development Kit (CDK) `ImageFromGoBuild` path automatically
embeds the parsed `BridgeConfig` already supplied to the facade. There is no
second config prop, separate Amazon S3 config asset, or runtime S3 download
grant. `BridgeYamlAsset(path)` names a local authoring input, not a runtime
S3 configuration source.

The Docker build context contains `initial-config-<rawSHA>.base64` as pure data.
`rawSHA` is the SHA-256 hash of the unencoded serialized document, not the
Base64 text. Changing those document bytes changes the image asset.

For a config-bearing image, the generated build:

1. Downloads the requested published package version through Go tooling.
2. Locates its owning module and copies that module to a writable build directory.
3. Requires the command's fixed `initial-config.base64` file and fills that
   file in the build copy with the staged payload.
4. Runs `go build` with the selected tags and small version/commit linker flags.

No Git checkout is needed, and the downloaded module cache is not modified.
Without an embedded document, `ImageFromGoBuild` still uses
`go install package@version`. Neither path puts config bytes in flags or
environment variables.

After compilation, the generated build runs
`/gobridge-filebased -initial-config-digest` and requires the result to match
the staged document's SHA-256 hash. A missing probe, failed probe, or mismatched
hash fails the image build. Custom commands must provide the fixed embed file,
consume it through `go:embed`, and support this probe when config is embedded.
An older package without that contract cannot silently produce an image
without the declared document.

A compatible, published `lib` module is a prerequisite for this versioned
build. Optional profile-family registration is also a prerequisite when the
document uses those families. Those publication and wiring tasks remain
pending; existing released versions are not evidence that this path is ready.
See the [image-source contract](cdk-constructs.md#runtime-image-source).

### Registry and ECR images

Amazon Elastic Container Registry (ECR) and other registry images are
consumer-built. CDK cannot modify an image supplied by `ImageFromRegistry` or
`ImageFromEcrRepository`. The consumer owns that image build. It can contain
its own embedded initial document, consume an existing target, or wait for
an operator to create the target.

`BridgeConfig` still declares the configuration for synth-time validation,
port mappings, resource dependencies, and Identity and Access Management
(IAM) grants. Supplying it does not copy it into a registry image and does
not overwrite the live target. Keep the declared resources consistent with
the image and the target; CDK grants access only to the declared resources.

## Strict creation

The shared orchestration is `config.Initialize`:

```go
func Initialize(
    ctx context.Context,
    target ports.ConfigStore,
    source ports.Loader,
    admit func(context.Context, *ports.BridgeConfig) error,
) error
```

It validates an existing target without reading the initial source. If the
target is absent, it validates the source, attempts strict creation, then
reloads and admits the winner. A nil return means the authoritative target
was loaded and admitted; it does not mean the data plane is running.
The caller owns writer authorization and the first-activation latch.

For example, a composition root can supply parsed inline input:

```go
source := parser.NewInlineSource(initialYAML, registry)
if err := config.Initialize(ctx, target, source, admit); err != nil {
    return err
}
```

The initial source implements the existing `ports.Loader`. The target may
implement the optional capability:

```go
CreateIfAbsent(ctx context.Context, cfg *ports.BridgeConfig) (bool, error)
```

This is `ports.ConfigInitializer`, separate from ordinary configuration
updates. Its result reports whether this caller created the document.
The boolean is meaningful only with a nil error. An error can leave the commit
outcome unknown: reread the target rather than overwriting or deleting it to
compensate.

1. Read the target through its loader.
2. Initialize only after a definitive document-absence result, and only during
   the initial-start phase of an authorized control process.
3. Validate the initial logical document without resolving its credential
   references into the stored copy.
4. Create atomically at target version 1, ignoring the source's version.
5. Reread the target and use the winning document, including when another
   writer won the creation race.

An existing invalid, versionless, or otherwise legacy document is still an
existing document. It never authorizes overwrite. A missing DynamoDB table is
a backend error, not an absent configuration item.
Only an outer classified `shared.ErrNotFound` authorizes creation. An invalid
document that wraps a missing-reference error is not an absent target.

Do not substitute `SaveIfVersion(..., 0)` for strict creation.
Compare-and-swap (CAS) at version zero can adopt a legacy versionless row;
`CreateIfAbsent` must refuse to replace that row. Targets without the strict
capability require operator creation; initialization must not fall back to
ordinary `Save`.

Only the control role initializes shared configuration. Worker configuration
access remains read-only, independently of whether a worker can hold a
message-processing lease.

### Snapshot ownership

`Initialize` gives validation and the optional `admit` callback an isolated
snapshot. Resolving credentials or changing defaults in that callback cannot
change the document published to the target.

Mutable custom plugin configs must implement the existing
`ports.FreezableConfig` capability. For example, a config containing a map or
slice must return an owned snapshot through `FreezePluginConfig`.
Deeply immutable scalar value configs, such as a value struct containing only
strings and integers, need not implement it. Other capability requirements,
such as durable-session identity, still apply.

The core copies blueprint-owned state without a serialization round trip and
does not reflect-clone adapter-owned values. This preserves logical credentials
and numeric condition types. Observed snapshots must not be mutated while in use.

## Configuration identities

The high-availability (HA) deployment baseline uses content identity, not the
version supplied by the embedded source. Both file and DynamoDB sources use
`bridge.DeploymentBaselineContentDigest`, which normalizes only the top-level
`BridgeConfig.Version` to zero for comparison. Every editable field remains
covered.

For example, an embedded document with `version: 17` creates an absent target
at version 1. Both sources can recognize that content as the admitted baseline.
The generation-zero committed artifact still stores version 1 and its full,
version-sensitive `bridge.ConfigArtifactDigest`; baseline recognition never
rewrites the artifact's version.

The `-initial-config-digest` build probe has a different purpose: it hashes the
embedded bytes, including their original version and formatting. It proves the
binary contains those bytes, not that a runtime target matches the HA baseline.

## Observation and runtime state

The existing watcher reports explicit observations through
`ports.ConfigObserver`: `ConfigPresent`, `ConfigMissing`, or `ConfigReadError`.
This extends the source's watch path; it does not introduce another polling
service. `ConfigObservation` carries the result and `ConfigObservationKind`
names its category.

`ConfigObserver.Observe` emits an initial snapshot followed by ordered
observations. `ConfigObservation.Sequence` orders observations, not persisted
config versions. A nil config or closed channel is not evidence of absence.
Observed missing-to-present transitions must not be coalesced.

`config.Manager.Observe(ctx)` observes exactly one authoritative `config.Layer`.
It finds `ports.ConfigObserver` on that layer's watcher or loader.
Overlays are intentionally unsupported because overlay absence has no defined
merge policy; unsupported layers return `shared.ErrNotSupported`.
The existing `ports.Watcher` and `Manager.Watch` APIs remain available.
Choose one observation mode for a manager run, not concurrent `Watch` and
`Observe` calls. A terminated observer becomes a read error and is restarted
with backoff, never treated as document absence.

| Observation | Before first activation | After first activation |
|---|---|---|
| Valid document | Validate and activate it through the normal apply path. | Apply it under the normal reload or cluster-rollout rules. |
| Definitive document absence | Remain idle and not ready; the optional initial-start initializer may create it. | Stop new intake. Standalone runtimes drain and return to idle; clustered runtimes require process replacement. Uncertain teardown also requires exit. |
| Timeout, authorization failure, or unavailable backend | Remain idle and report the error. | Keep the last successfully applied config and report degraded service. |
| Existing invalid document | Remain idle and report validation failure; do not overwrite it. | Reject it through normal validation and retain the last successful runtime with diagnostics. |

For ordinary standalone deletion, completed quiescence releases the old runtime;
a later valid document builds a new runtime in the same process.
For an activated clustered config, deletion signals process exit and replacement,
not a live transition to idle. This applies to clustered deployments with or
without the coordinated rollout barrier. Any uncertain teardown also exits.
Never keep the old runtime or a cached committed artifact processing after
confirmed absence. Liveness is not readiness: a valid control plane may be live
while the standalone data plane is idle and not ready.

While no runtime is active, `/deephealth` returns HTTP 503 with `empty: true`
and configuration-watch diagnostics. `config_watch.startup_pending: true`
identifies normal startup waiting or a positively classified transient fault.
It does not mark authorization failures, rejected configuration, terminal state,
or failures after first activation as pending. Deployment tooling can wait for
this state without hiding genuine configuration rejection.

After completing data-plane quiescence, the host calls `Manager.NotifyIdle()`.
It clears confirmed running version and fingerprint state, not a newer desired
snapshot that may already be queued. It does not drain the runtime itself.
The missing observation separately invalidates the old desired acknowledgement.

First activation is a process-lifetime latch, not a Full-readiness test. An
HA standby may have activated its configuration without
holding a lease or reaching Full readiness. Returning to idle in that same
process never rearms initialization. A fresh process may initialize an absent
target again; there is no operator-managed durable tombstone.

Watchers preserve the order of present, missing, and error observations. A
delete followed by recreation must not hide the deletion or discard the
recreated document because its version restarted at 1.

## Queue references in embedded documents

For Amazon Simple Queue Service (SQS), use a stable physical `queue_name`,
such as `orders-in`, or the optional `queue_tags` selector with
`queue_name_prefix`. Tags are an alternative to `queue_name` and `queue_url`;
a prefix requires tags. `bridgecfg.ValidateEmbeddedSQSConfig` rejects all
embedded `queue_url` values, including literal URLs, and rejects URL-shaped
binding addresses. Direct `queue_url` remains supported for non-embedded
configurations.

For a tag-selected sender, use `address: sqs:queue` on its binding. This is
`sqs.QueueAddress`: “use the sender's configured queue.” It does not select a
new queue for each message. The resolved target URL remains runtime-only.
For example, the sender and binding portion of a config can be:

```yaml
senders:
  - id: orders-out
    transport: sqs
    options:
      queue_tags:
        application: gobridge
        purpose: orders
      queue_name_prefix: orders-
bindings:
  - id: to-orders
    sender_id: orders-out
    address: sqs:queue
```

Receivers and senders need their own queue reference in `options`; they do not
inherit queue options from a session. A physical name may also be used as the
binding address when known.

Name lookup uses the existing SQS `GetQueueUrl` operation. Tag selection uses
the native SQS SDK: paginate `ListQueues`, narrow
by prefix when supplied, then call `ListQueueTags`. Selection is scoped to the
client's account and region:

- Exactly one matching queue: use it.
- No matching queue: retry and remain not ready.
- Multiple matching queues: report an ambiguity error.
- Permission or service errors: report the error, not “no match.”

CDK keeps `IQueue` handles for precise message-operation grants and resource
dependencies. Call `QueueRegistry.AddQueue` first, then
`BindQueueTags(name, tags, prefix)` to bind a selector to that handle.
`ResolveQueue` matches declarations to handles without making AWS calls;
runtime discovery independently checks actual queues.
For CDK-owned queues, `BindQueueTags` applies the tags. For imported queues,
the producer must already apply them. A prefix must match the physical name,
not the registry alias. `QueueRef.PhysicalName`, `QueueTags`, and
`QueueNamePrefix` expose these distinct values; see the
[CDK reference and example](cdk-constructs.md#sqs-references).
Only tag mode needs the additional discovery metadata reads.
No generic CloudFormation token resolver or two-phase deployment is required
by default. See [SQS IAM](iam.md#sqs-discovery-grants).

## Artifact visibility and secrets

Initial configuration may contain literal values, literal credentials, or
references such as `pms://gobridge/mqtt`. There is no blanket prohibition on
literal credentials. Choose deliberately: anyone who can read the binary or
image can recover embedded values.

The CDK builder and Phase 1 do not run a plaintext-secret scanner.
`bridgecfg.ScanForPlaintextSecrets` is an explicit utility for consumers
who want their own reference-only policy.

Base64 is an encoding, not secrecy. Build contexts, CDK assemblies, generated
payloads, writable module copies, build caches, and images can expose the document.
Apply access controls and retention rules to those artifacts. Credential
references reduce the secrets stored in artifacts; runtime resolution must not
write the resolved secrets back into the initial logical copy.

## Operator creation and rollout

Use authenticated `POST /api/v1/admin/config` to create an absent target.
Send a complete YAML or JSON document, not an overlay or transaction patch.
The application programming interface (API) uses the composition root's
registry-aware decoder, so plugin options are typed and unknown kinds fail.
Admission uses a separate decoded copy; resolved secrets cannot enter the
logical document written through `ports.ConfigInitializer.CreateIfAbsent`.

From another terminal, create a YAML document:

```bash
ADMIN_URL=http://127.0.0.1:8080
curl --fail-with-body -i -X POST \
  -H "X-API-Key: $GOBRIDGE_ADMIN_API_KEY" \
  -H "Content-Type: application/yaml" \
  --data-binary @initial.yaml "$ADMIN_URL/api/v1/admin/config"
```

Use `Content-Type: application/json` for JSON. The request body limit is 4 MiB;
the target backend may impose a smaller limit. Use TLS or another protected
connection when the admin service is not on loopback.

Creation always rereads the authoritative target. It never applies the
request's in-memory candidate or deletes a winning document to compensate
for an error. Responses with a config status include the observed `version`:

| HTTP status | Outcome | Operator action |
|---|---|---|
| `201` | `committed`: this request created the document. | Check applied config and readiness; persistence is not a Full-readiness guarantee. |
| `202` | `committed_not_applied`: the write succeeded, but in-band apply did not complete successfully. | Keep the target; inspect apply diagnostics and watch convergence. Do not retry creation as an update. |
| `202` | `commit_outcome_unknown`: creation returned an error, but the target could be reread. | Inspect the authoritative target and returned version before another action. |
| `409` | `configuration_exists`: another or earlier document won strict creation. | Read the existing document; use normal update procedures if it must change. |
| `503` | The creation outcome cannot be determined because the target could not be reread. | Inspect the repository and restore access; do not assume the write failed or delete the target. |

Malformed bodies return `400`; failed admission returns `422`. A read-only
worker returns `403`. A root without the strict target capability or typed
decoder returns `501`. An invalid existing target is still protected against
overwrite; it may cause the authoritative reread to fail.

For an existing target, use normal config transactions or an atomic external
writer. Cluster replacement-required changes still need the
[whole-cohort procedure](../runbooks/cluster-config-rollout.md); changing the
embedded document is not a rollout mechanism.

Managed-subscription baseline seeding and the HA generation-zero committed
artifact remain separate operations. Neither is removed by this initialization
contract. An S3 configuration adapter is deferred.
