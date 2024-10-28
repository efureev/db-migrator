# syntax=docker.io/docker/dockerfile:1.9

# e.g.: `docker build --rm --build-arg "BASE_IMAGE=alpine:latest" -f ./Dockerfile .`
ARG BASE_IMAGE="scratch"

ARG TARGET='local'
ARG APP_NAME='migrate'
ARG VERSION_TAG

# this build is being run in the native $BUILDPLATFORM platform.
# here you would do a cross-compilation.
FROM --platform=$BUILDPLATFORM golang AS builder

ARG TARGET
ENV TARGET=${TARGET}
ARG VERSION_TAG
ENV VERSION_TAG=${VERSION_TAG}
ARG APP_NAME
ENV APP_NAME=${APP_NAME}

ARG BUILDPLATFORM
ARG TARGETPLATFORM

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . ./

RUN bash -c "./build.sh ${TARGET}"

# this build is being run in $TARGETPLATFORM platform as defined in the
# buildx --platform argument.
FROM ${BASE_IMAGE}
ARG APP_NAME

LABEL \
    # Docs: <https://github.com/opencontainers/image-spec/blob/master/annotations.md>
    org.opencontainers.image.title="migrate" \
    org.opencontainers.image.description="migrate app (static binary file) in docker image" \
    org.opencontainers.image.url="https://github.com/efureev/db-migrator" \
    org.opencontainers.image.source="https://github.com/efureev/db-migrator" \
    org.opencontainers.image.vendor="efureev" \
    org.opencontainers.image.licenses="WTFPL"

COPY --from=builder /build/$APP_NAME /bin/$APP_NAME
# NB 65534:65534 is the uid:gid of the nobody:nogroup user:group.
# NB we use a numeric uid:gid to easy the use in kubernetes securityContext.
#    k8s will only be able to infer the runAsUser and runAsGroup values when
#    the USER intruction has a numeric uid:gid. otherwise it will fail with:
#       kubelet Error: container has runAsNonRoot and image has non-numeric
#       user (nobody), cannot verify user is non-root

USER 65534:65534

ENTRYPOINT ["/bin/migrate"]