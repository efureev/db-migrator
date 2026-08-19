# syntax=docker/dockerfile:1

# The toolchain version comes in as an argument so that the image and the
# project cannot drift apart quietly; CI passes the one from go.mod.
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies first, so that a change to the source does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# TARGETOS and TARGETARCH are supplied by buildx, which is what makes this
# multi-arch without a build matrix.
ARG TARGETOS
ARG TARGETARCH

# The package path is a string here that no compiler checks. Its agreement with
# the real one is checked by internal/buildinfo/ldflags_test.go, which reads
# this file — because in v1 exactly this string was wrong and every release
# shipped a binary that reported its version as "unknown".
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w \
      -X github.com/efureev/db-migrator/v2/internal/buildinfo.version=${VERSION} \
      -X github.com/efureev/db-migrator/v2/internal/buildinfo.commit=${COMMIT} \
      -X github.com/efureev/db-migrator/v2/internal/buildinfo.date=${DATE}" \
    -o /out/migrator ./cmd/migrator

# ── the shipped image ────────────────────────────────────────────────────────
#
# distroless static rather than alpine: this tool runs no other program, it only
# opens a TCP connection to PostgreSQL. No shell, no package manager, about two
# megabytes, with ca-certificates and tzdata already inside. The nonroot tag
# brings user 65532, so no adduser step and no root at runtime.
FROM gcr.io/distroless/static-debian12:nonroot AS distroless

ARG VERSION=dev

LABEL org.opencontainers.image.title="migrator" \
      org.opencontainers.image.description="SQL migrations for PostgreSQL" \
      org.opencontainers.image.source="https://github.com/efureev/db-migrator" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/migrator /usr/local/bin/migrator

# Where the operator mounts their migrations. The default differs from the
# binary's own (./migrations) and the difference lives in this one line rather
# than in Go code — v1 hard-coded "/migrations" in the config defaults, which
# surprised everybody running it outside a container.
ENV MIGRATOR_DIR=/migrations

WORKDIR /work
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/migrator"]
CMD ["--help"]

# ── the same binary with a shell ─────────────────────────────────────────────
#
# Published as <tag>-alpine, for an init container that wants
# sh -c "migrator up && something-else".
FROM alpine:3.22 AS shell

ARG VERSION=dev

LABEL org.opencontainers.image.title="migrator" \
      org.opencontainers.image.version="${VERSION}"

RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 65532 nonroot

COPY --from=build /out/migrator /usr/local/bin/migrator

ENV MIGRATOR_DIR=/migrations

WORKDIR /work
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/migrator"]
CMD ["--help"]
