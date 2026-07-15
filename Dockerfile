# syntax=docker/dockerfile:1

# GoBridge production runtime image.
#
# Builds the file-based deployment binary (gobridge-filebased) as a fully
# static, CGO-free executable and ships it on distroless/static as a non-root
# user. The image ships no shell, curl or wget on purpose: the container
# health check reuses the binary itself (`-healthcheck`), which probes the
# local monitor /live endpoint (503 once the runtime is terminal).
#
# The build context is the repository root because the binary's module
# (deployment/aws-filebased-config/lib) resolves the rest of GoBridge through
# relative `replace` directives (../../.. etc.). We build with GOWORK=off so
# the container build is reproducible from the module's own go.mod/go.sum
# rather than the workspace's go.work.

ARG BINARY_MODULE=deployment/aws-filebased-config/lib
ARG BINARY_PKG=./cmd/gobridge-filebased

# ---- build stage ------------------------------------------------------------
# Base image pinned to a top-level multi-platform OCI index digest (verified to
# include linux/amd64 AND linux/arm64) so a rebuild pulls the exact reviewed
# bytes instead of whatever the mutable tag points at today. The human-readable
# tag is retained for readability; Docker resolves the digest.
# Refresh + verify (records the current index digest and its platforms):
#   docker buildx imagetools inspect golang:1.25-bookworm --format '{{.Manifest.Digest}}'
#   docker buildx imagetools inspect golang:1.25-bookworm --raw | \
#     jq -r '.manifests[]|select(.platform!=null)|"\(.platform.os)/\(.platform.architecture)"'
# See DEVELOPMENT.md → "Base image digests" for the review workflow.
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
ARG BINARY_MODULE
ARG BINARY_PKG

# Version metadata (optional; passed by CI via --build-arg).
ARG VERSION=dev
ARG GIT_SHA=unknown

WORKDIR /src
# Copy the whole repository: the target module's replace directives point up
# and out to sibling modules across the repo.
COPY . .

ENV CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    cd "${BINARY_MODULE}" && \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.gitSHA=${GIT_SHA}" \
      -o /out/gobridge-filebased "${BINARY_PKG}"

# ---- runtime stage ----------------------------------------------------------
# distroless/static-debian12:nonroot runs as uid:gid 65532:65532 and contains
# only CA certificates, tzdata and /etc/passwd — no shell, no package manager.
# Pinned to a top-level multi-platform OCI index digest (verified to include
# linux/amd64 AND linux/arm64); refresh + verify it the same way as the build
# base (see DEVELOPMENT.md → "Base image digests").
FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b AS runtime

COPY --from=build /out/gobridge-filebased /usr/local/bin/gobridge-filebased

# Non-root by default (matches the CDK container User and the distroless user).
USER 65532:65532

# The health check reuses the binary; distroless has no curl/wget/shell.
# ECS overrides this with an equivalent HealthCheck.Command, but keeping it in
# the image makes `docker run` and CI smoke tests self-checking.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/gobridge-filebased", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/gobridge-filebased"]
