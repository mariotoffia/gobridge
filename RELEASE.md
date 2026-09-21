# Releasing GoBridge

## Overview

How to version, tag, and publish the multi-module workspace so external consumers
can use `go get` and `go install`. Development-side rules live in
[DEVELOPMENT.md — Module versioning & references](DEVELOPMENT.md#module-versioning--references).

## One version for everything

The single most important rule here, and the one everything else serves:
**every published module carries the same exact `vX.Y.Z`.**

There is no per-module versioning, no "only tag what changed", and no
compatibility matrix. A release is one train: every module is staged, tagged,
and verified from the same commit lineage, in dependency order, and the version
is only complete when the last module passes. A consumer who pins `v0.3.0` gets
the exact set that was built and tested together.

This costs a few redundant tags on modules that did not change. It buys the
elimination of an entire category of bug — the one where the core moves, an
adapter does not, and the mismatch only shows up in production. Do not
"optimise" it back into per-module versions.

## Canonical release graph

[`scripts/release/modules.json`](scripts/release/modules.json) is the only
hand-maintained published-module list and release-layer definition. The release
tool, Make targets, CI source preflight, and tag workflow all consume it. List
the graph without copying it into another file:

```bash
make release-modules RELEASE_FORMAT=tsv
make release-modules RELEASE_LAYER=1
```

The repository currently has **41 published modules**:

| Layer | Count | Contents |
|---|---:|---|
| 0 | 1 | Root module |
| 1 | 7 | Test helper modules under `testutil/` |
| 2 | 27 | Direct-root adapter/processor leaf modules, plus `deployment/aws/infra` |
| 3 | 3 | `adapters/aws/store`, `adapters/native/store`, and `httpapi` |
| 4 | 2 | `deployment/aws/cdk` and `deployment/aws/lib` |
| 5 | 1 | `cmd/gobridge` |

The published set is the root module, every module under `adapters/`,
`processors/` and `testutil/`, `httpapi`, `cmd/gobridge`, and the three AWS
deployment-profile modules `deployment/aws/{infra,lib,cdk}`. Everything else
under `tests/`, `scripts/`, and `deployment/` is internal-only and is never
tagged.

The test helper modules sit on layer 1 because they require only the root and
the adapters' tests require them; see [Test helper modules](#test-helper-modules).

The deployment-profile modules are published because an external CDK app writes
its own stack against the constructs, those constructs take `infra` types as
arguments, and the default `ImageFromGoBuild` image source builds the profile
command out of `lib` from the module proxy at the train version. All three must
resolve publicly or the documented quickstart in `docs/scenarios/cdk/` cannot
compile — and cannot build its image — outside this repository. `cdk` and `lib`
both sit above the layer-3 store aggregates they require, which is why
`cmd/gobridge` sits on layer 5 — the final module must be alone on the highest
layer.

Optional profile-family wiring must still be available for any family a bridge
config requests; a published `lib` tag does not by itself imply support for
every family. See
[CDK image sources](docs/aws-deployment/cdk-constructs.md#runtime-image-source).

**Pre-existing profile tags are not a usable train.** `infra` and `cdk` joined
the train at `v0.3.4` and carry real tags — `infra` `v0.3.4`-`v0.3.6`, `cdk`
`v0.3.4` and `v0.3.6` (no `v0.3.5`). `lib` has never been tagged at any version,
so no `v0.3.x` names a complete profile set. Those tags also predate the sealed
image sources by two weeks: `ImageFromGoBuild` does not exist in `cdk/v0.3.6`. They stay in place — policy 6 forbids moving or
deleting a tag — and they must not be referenced from documentation or consumer
instructions. The first usable profile version is the first train published
after this change.

## Test helper modules

The modules under `testutil/` start the brokers and emulators the test suite
needs and wait until they are ready: `mqttlocal` (Mosquitto), `rabbitmqlocal`
(RabbitMQ, AMQP 0-9-1), `artemislocal` (Apache Artemis, AMQP 1.0), `asblocal`
(Azure Service Bus emulator), `flocilocal` (Floci, an AWS emulator for SQS,
SSM, CloudWatch and more), `ddblocal` (DynamoDB Local), and `testcontent`
(sent-versus-received message verification). A project that builds its own
composition root can use them in its own integration tests:

```bash
go get github.com/mariotoffia/gobridge/testutil/mqttlocal@vX.Y.Z
```

The readiness helper `testutil/wait`, the Docker wrapper
`testutil/dockerexec`, the TCP fault-injection proxy `testutil/netfault` and
the TLS certificate generator `testutil/tlsgen` are packages of the root
module, so they come with `go get github.com/mariotoffia/gobridge@vX.Y.Z`.

**Their compatibility promise is lighter than the runtime modules'.** The
helpers carry the same version as everything else, so pin them to the version
of the runtime modules you use. A helper's exported Go API may change in any
release when the test suite needs it, with no deprecation period. Every such
change is listed in [CHANGELOG.md](CHANGELOG.md) under the release it ships
in; read that entry before moving a test suite to a new version.

The first helper tags are created by the first train published after this
change. No earlier version has them, so `go get …/testutil/<helper>@v0.4.1`
and older still fail.

## Policy

1. **Single stable version train.** Every published module uses the same exact
   `vX.Y.Z`. Prerelease and build metadata tags are rejected.
2. **One tag per module path.** Root uses `vX.Y.Z`; a nested module uses
   `<module-dir>/vX.Y.Z`.
3. **Dependency layers are strict.** A published sibling requirement must point
   to a lower layer. Before a layer is tagged, every lower-layer tag in that
   version train must exist and be an ancestor of the candidate commit.
4. **No replacements or excludes in published modules.** Local development
   resolution belongs in `go.work`; release and external-consumer gates reject
   local and versioned `replace` directives plus every `exclude` directive.
5. **No unresolved placeholders.** Exact `v0.0.0`, all-zero or malformed
   pseudo-versions, undeclared repository siblings, and versions outside the
   selected train fail the strict gate.
6. **Never move a module tag.** A failed public module release is corrected with
   a new patch train, not by deleting or recreating a tag.

## Required GitHub tag ruleset

The release workflow rejects any tag push unless GitHub reports
`created=true`, `deleted=false`, `forced=false`, and `ref_protected=true`.
The repository **tag ruleset** enforces this and must stay in place:

- target patterns `v*` and `**/v*` (the verifier remains the authoritative
  published-module allow-list);
- restrict tag creation, updates, and deletions;
- grant bypass/creation authority only to the approved release principals;
- do not permit force updates or deletion after creation.

The event check is the first job, before checkout. Every privileged boundary
re-resolves both lightweight and annotated tags from `origin` with
`git ls-remote`, peels annotated tags, and requires the remote commit to remain
the original validated `github.sha`. A disappeared or moved tag fails GitHub
Release creation.

## Verification modes

The source-safe gate runs on every CI build and validates the release DAG plus
the tooling itself. It reports the in-repo manifest inventory (local `replace`
directives, `v0.0.0` requirements) — those are the **development** shape and are
expected on `main`; the release tool strips them per-tag at publish time:

```bash
make verify-release-preparation
```

The release-strict gate verifies an actually-published train. It requires the
matching tags to exist, so run it only for a version that has been released:

```bash
make verify-published-modules RELEASE_VERSION=v0.3.0
```

It verifies every declared module with the workspace disabled:

```text
go mod download
go mod verify
go build ./...
```

The gate proves the module is **consumable**: every module in its graph is
fetchable from the public proxy, the checksums match, and the replace-free
source compiles against those exact versions. It deliberately does not run the
module's tests. A release tag's commit differs from `main` only in `go.mod` and
`go.sum` — the Go source is identical — so `make test` has already run them on
this code, and a consumer never compiles them. Running them here imported CI's
flakes into an irreversible step without testing anything new about the
published artifact. `go mod download` with no arguments already fetches the
whole graph, test dependencies included, so `go build` adds no downloads; it
only proves the fetched versions compile together.

Each pushed module tag runs the same static checks and commands for that module.
The final `cmd/gobridge` tag additionally repeats the strict gate for all
modules. CI runs `make verify-release-preparation`, which validates the release
DAG and tooling against the source tree; the strict public-resolution gate
belongs to the tagged release workflow, not to CI.

## Release procedure

Every release runs this same bottom-up train — there is no separate
"first release" path. Run it on a dedicated `release/*` branch from a clean
checkout with `git`, `gh`, Go 1.25+, and registry access. Replace the example
version with the approved stable train.

In practice you invoke it as one command (`make release VERSION=vX.Y.Z
CONFIRM=1`, see [MODULES.md §3](MODULES.md#3-cut-a-release-make-it-go-get-able));
the sections below document what that command does at each step.

### 1. Define proxy and workflow waits

The proxy-only wait prevents the next layer from racing a stale cache. The
release workflow itself uses `https://proxy.golang.org,direct`, so direct VCS is
an allowed fallback during its strict gate.

```bash
set -euo pipefail
VERSION=v0.3.0
RELEASE_BRANCH="release/${VERSION}"

wait_for_proxy() {
  module="$1"
  until GOWORK=off GOPROXY=https://proxy.golang.org \
    go list -m "${module}@${VERSION}"; do
    echo "waiting for proxy.golang.org: ${module}@${VERSION}" >&2
    sleep 15
  done
}

wait_for_release_workflow() {
  tag="$1"
  run_id=""
  for _ in $(seq 1 30); do
    run_id="$(gh run list --workflow release.yml --event push --branch "$tag" \
      --limit 1 --json databaseId --jq '.[0].databaseId // empty')"
    if [ -n "$run_id" ]; then
      gh run watch "$run_id" --exit-status
      return
    fi
    sleep 5
  done
  echo "release workflow did not appear for $tag" >&2
  return 1
}
```

### 2. Release the root

The staging target rewrites only declared repository dependencies, removes local
replacements, runs `GOWORK=off go mod tidy`, and runs the strict pre-tag checks.
The root currently has no forbidden manifest entry, so it may produce no diff.

```bash
make stage-published-module RELEASE_MODULE=. RELEASE_VERSION="$VERSION"

if ! git diff --quiet -- go.mod go.sum; then
  git add go.mod go.sum
  git commit -m "release: root ${VERSION}"
fi

git tag "$VERSION"
git push origin "$VERSION"
wait_for_release_workflow "$VERSION"
wait_for_proxy github.com/mariotoffia/gobridge
```

### 3. Stage, tag, and push each dependency layer

This dependency-ordered stage/tag/push/wait loop is mechanized by
[`scripts/release/run.sh`](scripts/release/run.sh), invoked as `make release
VERSION=vX.Y.Z CONFIRM=1`. See [MODULES.md §3](MODULES.md#3-cut-a-release-make-it-go-get-able).
Run `make release VERSION=vX.Y.Z` first (dry-run) to review the per-layer plan.
The surrounding sections (§1 waits, §2 root above; §4 smoke below) document what
each step does; `run.sh` performs them in order and must not be bypassed to
retag.

Layer 1 is the seven test-helper modules. Each requires only the root, and
the adapters' tests require them, so they are tagged and visible on the proxy
before any adapter is staged.

Propagation is not uniform. A leaf module appears on proxy.golang.org in about
a minute; `adapters/aws/store` and `adapters/native/store`, whose directories
contain nested modules, have consistently taken 15 to 20. The verifier's
propagation budget is therefore 20 minutes, and a failed layer workflow gets
one re-run before the layer dies — a tag cannot be re-pushed, so a run that
failed on propagation alone must not cost an entire new version train. A
genuine defect still fails twice and stops the train.

No layer can start until every tag in the layer below it is green and visible,
so the final `cmd/gobridge` tag is reached only after both layer-4 modules,
`deployment/aws/cdk` and `deployment/aws/lib`, which in turn wait on all three
layer-3 tags. If a tagged workflow fails, stop. Do not retag; diagnose and
start a new patch train.

Once the last layer is green, push the release branch itself. Every per-module
release commit has already reached `origin` as part of a tag, but the branch
ref is what this project keeps permanently — a `release/*` branch is never
deleted — so pushing it after the final layer leaves the remote branch pointing
at the last release commit instead of a mid-train one.

```bash
git push origin "HEAD:refs/heads/${RELEASE_BRANCH}"
```

### 4. Final public proof

The stable `cmd/gobridge/vX.Y.Z` workflow runs this only after the complete
strict train succeeds:

```bash
make smoke-released-modules RELEASE_TAG="cmd/gobridge/${VERSION}"
```

The tool first retries a **proxy-only** pass with
`GOPROXY=https://proxy.golang.org` for bounded tag propagation, then repeats a
separate **direct-only** pass with `GOPROXY=direct`. Every attempt has a fresh
`HOME`, `GOPATH`, module/build cache, and `GOBIN`; system/global Git config is
disabled. Both passes retain checksum-database verification, bind Paho, all
three deployment-profile modules, and `cmd/gobridge` `Origin.Hash` to their
exact local tag commits, and run:

```text
go mod init example.com/gobridge-release-smoke
go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho@vX.Y.Z
go list github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho
go get github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridge@vX.Y.Z
go build github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridge
go install github.com/mariotoffia/gobridge/deployment/aws/lib/cmd/gobridge-aws@vX.Y.Z
go install github.com/mariotoffia/gobridge/cmd/gobridge@vX.Y.Z
```

It rejects every `replace` or `exclude` directive in resolved module manifests
and in the generated consumer go.mod.

The `lib` module is resolved *and* its command is installed. Nothing a consumer
writes imports it, so resolution proves only that the tag exists and that its
published manifest carries no `replace`. What a consumer actually runs is a
`go build` of `lib/cmd/gobridge-aws` inside the `ImageFromGoBuild` Docker
build at deploy time — after a stack update has begun. The strict per-module
gate compiles that tree from the staged manifest, but only this install
compiles it from the published module zip, which is the artifact the consumer
gets. It runs in module-agnostic mode, so it needs no `go.sum` entry in the
generated consumer manifest.

The CDK steps **build** rather than list. `cdk` is not in `cmd/gobridge`'s
dependency graph, so nothing else in the train compiles it from outside the
repository, and resolution alone would not catch a published manifest that no
longer satisfies the constructs' own imports. `gobridge` is the one package a
consumer's stack imports, and it imports every facade, the config and image
sources, the ALB attachment, the alarms and `ssmexports`, so building it
compiles the whole public surface from the path a reader would hit first.

The CDK steps fetch the **package** path, not the module path. `go get
module@version` records the requirement but not the `go.sum` entries for what
that module's own code imports, so the build that follows fails on every
missing sum. The Paho pair avoids this because `go list` needs no build
dependencies and `go install pkg@version` resolves in module-agnostic mode.

The pre-1.0 root-only tags `v0.1.0` and `v0.2.0` predate this policy and have no
nested module tags; they are not consumable and this proof does not apply to
them. `v0.3.0` is the first complete train.

## Initial configuration artifacts

The runtime binary can embed its initial configuration; there is no maintained
configuration-seeder image, publication job, image pin, or update command.
The previously published Docker Hub artifact is not deleted by this change,
but current deployments have no runtime dependency on it.

Consumer builds may embed YAML or JSON with `INITIAL_CONFIG_FILE`. Treat the
resulting binary, image, build context, and cache as copies of that document.
Literal credentials are permitted; Base64 does not conceal them.
See [initial configuration](docs/aws-deployment/config-initialization.md).

Both command packages must publish the fixed `initial-config.base64` file,
empty by default, and consume it with `go:embed`. Config-bearing CDK builds
download the requested package through Go tooling, copy its owning module to
a writable directory, fill the embed file, then build and verify its digest.
No Git checkout or payload-bearing flags/environment are used. Local Make and
Docker builds use the standard-library `scripts/buildconfig` overlay instead.

## No container image

The release train ends at the `cmd/gobridge` tag. Nothing in it builds, pushes,
scans, or promotes a container image, and the project publishes none. GitHub
Releases remain per-module; the final command release is created only after
strict train validation and both external consumer resolution passes succeed.

Consumers build their own image. On AWS a CDK facade with no `Image` set builds
the profile command from the published `deployment/aws/lib` module during
`cdk deploy` and pushes it into the account's own ECR asset repository; off AWS,
`deployment/kubernetes/Dockerfile` (or the repository root `Dockerfile`) builds
one for the consumer's registry. Pin whichever you run by digest —
[pin images by digest](docs/container-deployment.md#pin-images-by-digest).

## Production approval

Production approval is a separate post-merge, credentialed gate. Deploy the AWS
DynamoDB HA fixture in the protected target environment, stop the verified
leaseholder, collect the required warm/cold failure-to-Full samples, and retain
the CloudWatch evidence described in
[the credentialed proof](docs/aws-deployment/topologies.md#credentialed-failover-proof).
Set `GOBRIDGE_INT_VERSION` to a profile `lib` version from a published train.
No released train has published `lib` yet.
The fixtures build per-fixture embedded images through `ImageFromGoBuild`;
registry-image overrides are not accepted. Record each built image digest with
its module version and the proof evidence. The source-tag workflow cannot
supply that repository-specific AWS account, VPC, broker, secrets, or release
role. Do not describe or promote a release train as production-approved until
this external proof and the remaining controls in the production-readiness
release sequence are complete.
