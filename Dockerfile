# syntax=docker/dockerfile:1

# One file, three images. `local.services` in infra/terraform/locals.tf defines
# ingest, api and worker, ecr.tf gives each its own registry, and the only thing
# that differs between them is which package under ./cmd is built - so the
# alternative was three files differing by one word each.

ARG GO_VERSION=1.25

# The build stage runs on the machine doing the building and cross-compiles to
# the target, rather than emulating the target to run a native compiler on it.
# Go needs nothing but an environment variable to do this, and qemu-emulating an
# arm64 toolchain on an x86 runner to avoid setting it is the slow way to get
# the same binary.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies before source, because the layer cache is keyed on what changed.
# go.sum moves when a dependency does; internal/ moves several times an hour.
# Copying both at once would re-download the module graph for every edit.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG SERVICE
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "$SERVICE" || { echo "build with --build-arg SERVICE=ingest|api|worker"; exit 2; } && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/service ./cmd/${SERVICE}

# ---------------------------------------------------------------------------

FROM scratch

# scratch has no certificate store, and that is the failure this line exists to
# prevent. Every one of these services talks TLS to something outside the task -
# S3 and SQS at minimum, Anthropic and Voyage from the agent path - and without
# a root bundle all of it fails with `x509: certificate signed by unknown
# authority`, which reads like a broken endpoint rather than a missing file.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/service /service

# Numeric because scratch has no /etc/passwd for a name to resolve against.
# 65532 is the conventional unprivileged id; the container needs no filesystem
# of its own, so there is nothing for root to be root over.
USER 65532:65532

ENTRYPOINT ["/service"]
