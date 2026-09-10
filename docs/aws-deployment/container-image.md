# Container Image

Building the production image and keeping the registry from growing without
bound.

Part of the [AWS Deployment Overview](overview.md).

---

An external CDK app usually needs none of this. `gobridgecdk.ImageFromGoBuild`
builds the same image from published modules during `cdk deploy`, so a consumer
neither clones this repository nor runs `docker build`
([CDK image sources](cdk-constructs.md#runtime-image-source)). This page covers
the repository's own build: local development, CI, and the pipelines that
produce a registry image for `ImageFromRegistry` or `ImageFromEcrRepository`.

## Production Dockerfile

The repository ships a multi-stage `Dockerfile` at the root that builds the
`gobridge-filebased` binary as a static, **CGO-free** executable — the SQLite
store uses `modernc.org/sqlite`, which is pure Go, so there is no cgo and no
`CGO_ENABLED=1` — and ships it on `distroless/static-debian12:nonroot`:

The abbreviated example below shows the image layout. Use the repository's
actual Dockerfile for build metadata, cache mounts and optional initial-config
embedding.

```dockerfile
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod
RUN cd deployment/aws-filebased-config/lib && \
    go build -trimpath -ldflags="-s -w" \
      -o /out/gobridge-filebased ./cmd/gobridge-filebased

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b AS runtime
COPY --from=build /out/gobridge-filebased /usr/local/bin/gobridge-filebased
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/gobridge-filebased", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/gobridge-filebased"]
```

Build from the repository root — the binary module resolves the rest of
GoBridge through relative `replace` directives (`docker build -t
gobridge-filebased:latest .`).

Key points:

- The **EFS access point** enforces the POSIX file identity, so the container
  runs as the distroless nonroot user (65532), not UID 1000.
- **CA certificates** ship in the distroless base for TLS to AWS services.
- The image has **no shell, curl, or wget**, so the health check reuses the
  binary's `-healthcheck` flag (which probes the local monitor `/live`
  endpoint) instead of an HTTP client.
- **Base images are pinned by digest.** Both `FROM` lines carry a top-level
  multi-platform OCI index digest (verified to include `linux/amd64` and
  `linux/arm64`), so a rebuild pulls the exact reviewed bytes rather than
  whatever the mutable tag points at. Refresh a digest only through a reviewed
  change — see [DEVELOPMENT.md](../../DEVELOPMENT.md) (Base image digests) for the
  resolve/verify commands. A source rebuild is reproducible only to the extent
  the pinned bases, the locked per-module `go.sum`, and the Go toolchain are
  fixed; nothing here claims bit-for-bit reproducibility beyond those facts.

## Embedded initial configuration

Supply a YAML or JSON file inside the build context:

```sh
docker build --build-arg INITIAL_CONFIG_FILE=config/initial.yaml \
  -t gobridge-filebased:local .
```

The build uses native Go file embedding. The document is not carried in compiler
arguments or environment variables, and the original command source is not
modified. The image build verifies the binary's `-initial-config-digest` output
against the supplied document.

This is an initial value, not a continuous configuration source. An existing
target document wins; an empty target can be initialized at startup. See
[Initial configuration](config-initialization.md) for creation, idle-state and
credential-value behavior.

## ECR Lifecycle Policy

We recommend keeping the **last 10 tagged images** and expiring untagged
images after 1 day. This prevents unbounded storage growth while retaining
enough history for rollbacks.

```json
{
  "rules": [
    {
      "rulePriority": 1,
      "description": "Expire untagged images after 1 day",
      "selection": {
        "tagStatus": "untagged",
        "countType": "sinceImagePushed",
        "countUnit": "days",
        "countNumber": 1
      },
      "action": { "type": "expire" }
    },
    {
      "rulePriority": 2,
      "description": "Keep last 10 tagged images",
      "selection": {
        "tagStatus": "tagged",
        "tagPrefixList": ["v"],
        "countType": "imageCountMoreThan",
        "countNumber": 10
      },
      "action": { "type": "expire" }
    }
  ]
}
```

---
