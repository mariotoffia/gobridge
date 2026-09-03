# syntax=docker/dockerfile:1

# GoBridge Kubernetes profile image.
#
# Builds the reference composition root (cmd/gobridge: MQTT transport, native
# memory/SQLite stores, file:// credentials, in-process HTTP API) as a fully
# static, CGO-free executable and ships it on distroless/static as the
# non-root user. There is no shell, curl or wget in the image: kubelet probes
# the monitor port over HTTP (deployment/kubernetes/gobridge.yaml).
#
# Build from the REPOSITORY ROOT — the module resolves the rest of GoBridge
# through relative `replace` directives — with GOWORK=off so the build is
# reproducible from the module's own go.mod/go.sum:
#
#   docker build -f deployment/kubernetes/Dockerfile -t <registry>/gobridge:<tag> .
#
# A custom composition root (one that registers more transports or stores)
# builds with the same file: point BINARY_MODULE at its module directory.
#
# Base images are pinned to the same multi-platform OCI index digests as the
# root Dockerfile; refresh them only through the review in DEVELOPMENT.md
# ("Base image digests").

ARG BINARY_MODULE=cmd/gobridge

# ---- build stage ------------------------------------------------------------
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
ARG BINARY_MODULE

WORKDIR /src
COPY . .

ENV CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    cd "${BINARY_MODULE}" && \
    go build -trimpath -ldflags="-s -w" -o /out/gobridge . && \
    mkdir -p /out/state

# ---- runtime stage ----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b AS runtime

COPY --from=build /out/gobridge /usr/local/bin/gobridge

# The state mount point exists in the image and is owned by the runtime user,
# so an empty volume mounted there inherits that ownership: the SQLite
# managed-subscription store insists on an owner-controlled 0700 parent and
# refuses to start on a root-owned directory.
COPY --from=build --chown=65532:65532 /out/state /var/lib/gobridge

USER 65532:65532
WORKDIR /var/lib/gobridge

ENTRYPOINT ["/usr/local/bin/gobridge"]
