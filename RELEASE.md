# Releasing GoBridge

How to version, tag, and publish the multi-module workspace so external consumers
can use `go get` and `go install`. Development-side rules live in
[DEVELOPMENT.md — Module versioning & references](DEVELOPMENT.md#module-versioning--references).

> **Current state (2026-07-16): this policy is not yet applied publicly.**
> Published go.mod files still contain local `replace` directives, exact
> `v0.0.0` requirements, and all-zero pseudo-versions. Only root tags `v0.1.0`
> and `v0.2.0` exist. There are no path-prefixed module tags, so clean external
> consumption still fails. Do not present the installation examples as working
> until the first release procedure and external-consumer smoke gate succeed.

## Canonical release graph

[`scripts/release/modules.json`](scripts/release/modules.json) is the only
hand-maintained published-module list and release-layer definition. The release
tool, Make targets, CI source preflight, and tag workflow all consume it. List
the graph without copying it into another file:

```bash
make release-modules RELEASE_FORMAT=tsv
make release-modules RELEASE_LAYER=1
```

The repository currently has **31 published modules**:

| Layer | Count | Contents |
|---|---:|---|
| 0 | 1 | Root module |
| 1 | 26 | Direct-root adapter/processor leaf modules |
| 2 | 3 | `adapters/aws/store`, `adapters/native/store`, and `httpapi` |
| 3 | 1 | `cmd/gobridge` |

The remediation plan expected 24 layer-1 modules. Repository truth is 26:
`dynamodbmanagedsubscriptions` and `sqlitemanagedsubscriptions` are additional
store implementation modules and therefore precede their aggregate modules.

The published set is the root module, every module under `adapters/` and
`processors/`, `httpapi`, and `cmd/gobridge`. Modules under `tests/`,
`testutil/`, `scripts/`, and `deployment/` are internal-only and are never
tagged. The manifest declares only the test-helper modules required to compile
published-module tests as pseudo-version bootstrap exceptions; that does not
make them tagged releases.

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
6. **Internal helper pseudo-versions come from Go.** Never construct a
   timestamp/hash manually. Push the bootstrap commit first, then derive every
   version with `go list -m -json <module>@<commit>`. The release tool verifies
   the returned origin commit and downloaded helper go.mod.
7. **Never move a module tag.** A failed public module release is corrected with
   a new patch train, not by deleting or recreating a tag. Container releases
   have no semver registry tag; their immutable identity is the recorded digest.

## Required GitHub tag ruleset

The release workflow rejects any tag push unless GitHub reports
`created=true`, `deleted=false`, `forced=false`, and `ref_protected=true`.
Configure a repository **tag ruleset** before the first train:

- target patterns `v*` and `**/v*` (the verifier remains the authoritative
  published-module allow-list);
- restrict tag creation, updates, and deletions;
- grant bypass/creation authority only to the approved release principals;
- do not permit force updates or deletion after creation.

The event check is the first job, before checkout. Every privileged boundary
re-resolves both lightweight and annotated tags from `origin` with
`git ls-remote`, peels annotated tags, and requires the remote commit to remain
the original validated `github.sha`. A disappeared or moved tag fails GitHub
Release creation, digest publication, digest-asset upload, and `latest`
promotion.

## Verification modes

The source-safe gate is green before migration while explicitly reporting the
known debt:

```bash
make verify-release-preparation
```

At this revision it reports 31 modules and the following **published-manifest**
inventory:

- 72 local replacement entries;
- 57 exact `v0.0.0` requirements;
- 10 all-zero pseudo-versions;
- 0 malformed pseudo-versions.

It also reports five internal helper root requirements that must be changed
during bootstrap. Their five local replacements may remain because those
modules are internal-only; dependency-module replacements are ignored by Go.

The release-strict gate intentionally fails now:

```bash
make verify-published-modules RELEASE_VERSION=v0.3.0
```

After migration and all matching tags exist, that command verifies every
declared module with the workspace disabled:

```text
go mod download
go mod verify
go build ./...
go test -count=1 ./...
```

Each pushed module tag runs the same static checks and commands for that module.
The final `cmd/gobridge` tag additionally repeats the strict gate for all
modules. CI runs only `make verify-release-preparation` before the first release;
it does not carry a permanently red public-resolution job.

## First release procedure

This is a one-time bottom-up migration. Run it on a dedicated release branch
from a clean checkout with `git`, `gh`, Go 1.25+, and registry access. Replace
the example version only with the approved stable train.

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

### 3. Bootstrap internal test helpers from a reachable commit

The helpers that import root-owned test support must require the root version
just published. Their local replacements remain for workspace development.
Push the commit before deriving pseudo-versions; an unpushed commit is rejected.

```bash
make stage-release-bootstrap RELEASE_VERSION="$VERSION"
git add testutil/*/go.mod
git commit -m "release: bootstrap test helpers for ${VERSION}"
git push origin "HEAD:refs/heads/${RELEASE_BRANCH}"

BOOTSTRAP_COMMIT="$(git rev-parse HEAD)"
make derive-release-bootstrap \
  RELEASE_VERSION="$VERSION" \
  RELEASE_BOOTSTRAP_COMMIT="$BOOTSTRAP_COMMIT"
```

`derive-release-bootstrap` executes the authoritative Go query for every helper
and prints the exact returned pseudo-version. `stage-published-module` repeats
those queries before writing a helper requirement; no timestamp or abbreviated
hash is accepted from operator input.

### 4. Stage, tag, and push each dependency layer

This dependency-ordered stage/tag/push/wait loop is mechanized by
[`scripts/release/run.sh`](scripts/release/run.sh), invoked as `make release
VERSION=vX.Y.Z CONFIRM=1`. See [MODULES.md §3](MODULES.md#3-cut-a-release-make-it-go-get-able).
Run `make release VERSION=vX.Y.Z` first (dry-run) to review the per-layer plan. The
surrounding sections (§1 waits, §2 root, §3 bootstrap above; §5 smoke below) document what each step does;
`run.sh` performs them in order and must not be bypassed to retag.

Layer 2 cannot start until all layer-1 tags are green and visible. The final
module cannot start until all three layer-2 tags are green and visible. If a
tagged workflow fails, stop. Do not retag; diagnose and start a new patch train.

### 5. Final public proof

The stable `cmd/gobridge/vX.Y.Z` workflow runs this only after the complete
strict train succeeds:

```bash
make smoke-released-modules RELEASE_TAG="cmd/gobridge/${VERSION}"
```

The tool first retries a **proxy-only** pass with
`GOPROXY=https://proxy.golang.org` for bounded tag propagation, then repeats a
separate **direct-only** pass with `GOPROXY=direct`. Every attempt has a fresh
`HOME`, `GOPATH`, module/build cache, and `GOBIN`; system/global Git config is
disabled. Both passes retain checksum-database verification, bind Paho and
`cmd/gobridge` `Origin.Hash` to their exact local tag commits, and run:

```text
go mod init example.com/gobridge-release-smoke
go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho@vX.Y.Z
go list github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho
go install github.com/mariotoffia/gobridge/cmd/gobridge@vX.Y.Z
```

It rejects every `replace` or `exclude` directive in resolved module manifests
and in the generated consumer go.mod. Do not run this proof against `v0.1.0`
or `v0.2.0`; the required nested tags do not exist.

## Image publication

Only a successful stable `cmd/gobridge/vX.Y.Z` workflow can publish an image.
Root, adapter, processor, deployment, internal, and prerelease tags cannot enter
the image job. Image publication creates a **release candidate**, not production
approval.

The workflow uses immutable action commit SHAs, Buildx v0.35.0,
`moby/buildkit:v0.31.1@sha256:6b59b7df63a8cb9902736f9ddf7fcff8261613d3e7449b8ea8b7537fc399c03a`,
and
`tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0`.
It:

1. pushes the multi-platform image **by digest only** with BuildKit exporter
   `push-by-digest=true,name-canonical=true`, SBOM, and
   `provenance: mode=max`; no candidate or semver container tag is created;
2. validates the image index digest and requires exactly one runnable
   `linux/amd64` child and one runnable `linux/arm64` child;
3. scans **both exact child digests**, never a mutable tag, using Trivy Action
   v0.36.0 pinned to commit
   `ed142fd0673e97e23eac54620cfb913e5ce36c25`;
4. fails on any HIGH or CRITICAL OS/library vulnerability, including unfixed
   findings;
5. records `ghcr.io/mariotoffia/gobridge@sha256:...` in the workflow summary
   and attaches `gobridge-image-digest.txt` to the matching command GitHub
   Release after the image succeeds;
6. immediately queries protected remote Git tags inside the serialized image
   job and moves the sole mutable tag, `latest`, only when this release is the
   highest stable
   `cmd/gobridge/vX.Y.Z`; delayed older jobs leave `latest` unchanged.

Reruns first fetch `gobridge-image-digest.txt` from the exact command GitHub
Release. If no association exists, the workflow builds and publishes by digest.
If one exists, the workflow resumes from that immutable recorded digest without
rebuilding. It re-inspects the recorded index in GHCR, requires the exact two
runnable platform children, and rescans both exact child digests before
`latest` can move. This is deliberate: maximum BuildKit provenance contains
build-specific attestation metadata, so two valid builds need not have the same
top-level OCI index digest. Authentication, network, malformed asset, wrong
image, a missing registry digest, or a failed child scan fails closed. Before
upload, the workflow fetches the asset again: the same digest is an idempotent
association, a different digest fails, and a duplicate-name upload failure
closes the final race.

Release permissions are split by boundary. The build/resume/dual-scan job has
`contents: read` plus `packages: write`. A minimal pinned `github-script` job
has only `contents: write`, performs no checkout, Docker, BuildKit, QEMU, Trivy,
or repository command, and persists/revalidates the exact digest asset. A final
serialized `latest` job has `contents: read` plus `packages: write`, revalidates
the associated registry digest and protected highest tag, and performs no build
or scan. No job combines `contents: write` with `packages: write`.

GHCR does not document immutable tag enforcement or conditional OCI tag
creation, so a version-to-image association is **only** the digest asset on the
command GitHub Release, never `ghcr.io/...:vX.Y.Z`. `latest` is not part of the
build and is promoted from the exact scanned digest without rebuilding. GitHub
Releases remain per-module; the final command release is created only after
strict train validation and both external consumer resolution passes succeed.

Production approval is a separate post-merge, credentialed gate. Deploy the AWS
DynamoDB HA fixture in the protected target environment, stop the verified
leaseholder, collect the required warm/cold failure-to-Full samples, and retain
the CloudWatch evidence described in
`deployment/aws-filebased-config/README.md`. The source-tag workflow cannot
supply that repository-specific AWS account, VPC, broker, secrets, or release
role. Do not describe or promote the published image as production-approved
until this external proof and the remaining controls in the production-readiness
release sequence are complete.
