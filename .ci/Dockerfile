ARG VERSION_DEFAULT=0.0.0
ARG BUILD_TIME='-'
FROM golang AS builder

ARG VERSION_DEFAULT
ENV VERSION=$VERSION_DEFAULT
ENV BUILD_TIME=$BUILD_TIME

ENV GOPATH /go
ENV PATH ${GOPATH}/bin:$PATH
ENV GO111MODULE=on
ENV CGO_ENABLED 0

RUN echo "version: ${VERSION}"
RUN echo "build time: ${BUILD_TIME}"

WORKDIR /app
COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN /app/build.sh

RUN ls -la /app/build

FROM alpine
ARG VERSION_DEFAULT

LABEL maintainer="Eugene Fureev <fureev@gmail.com>"
LABEL author="Eugene Fureev <fureev@gmail.com>"
LABEL version="${VERSION_DEFAULT}"
LABEL build_time="${BUILD_TIME}"

COPY --from=builder "/app/build/migrator.linux.x64" /app/migrator
COPY --from=builder /app/config.example.toml /app/config.toml

WORKDIR /app
