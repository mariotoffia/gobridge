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

ARG GO_VERSION=1.25
ARG BINARY_MODULE=deployment/aws-filebased-config/lib
ARG BINARY_PKG=./cmd/gobridge-filebased

# ---- build stage ------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build
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
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/gobridge-filebased /usr/local/bin/gobridge-filebased

# Non-root by default (matches the CDK container User and the distroless user).
USER 65532:65532

# The health check reuses the binary; distroless has no curl/wget/shell.
# ECS overrides this with an equivalent HealthCheck.Command, but keeping it in
# the image makes `docker run` and CI smoke tests self-checking.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/gobridge-filebased", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/gobridge-filebased"]
