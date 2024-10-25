# syntax=docker.io/docker/dockerfile:1.9

ARG TARGET='local'
ARG VERSION_TAG
# this build is being run in the native $BUILDPLATFORM platform.
# here you would do a cross-compilation.
FROM --platform=$BUILDPLATFORM golang AS builder
ARG TARGET
ENV TARGET=${TARGET}
ARG VERSION_TAG
ENV VERSION_TAG=${VERSION_TAG}
ARG BUILDPLATFORM
ARG TARGETPLATFORM
WORKDIR /build

COPY build.sh go.* *.go ./

RUN go mod download

RUN bash -c "./build.sh ${TARGET}"

# this build is being run in $TARGETPLATFORM platform as defined in the
# buildx --platform argument.
FROM alpine
COPY --from=builder /build/migrator /app/
# NB 65534:65534 is the uid:gid of the nobody:nogroup user:group.
# NB we use a numeric uid:gid to easy the use in kubernetes securityContext.
#    k8s will only be able to infer the runAsUser and runAsGroup values when
#    the USER intruction has a numeric uid:gid. otherwise it will fail with:
#       kubelet Error: container has runAsNonRoot and image has non-numeric
#       user (nobody), cannot verify user is non-root

USER 65534:65534

ENTRYPOINT ["/app/migrator"]