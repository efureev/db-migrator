ARG VERSION_DEFAULT=0.0.0
FROM golang AS builder

ARG VERSION_DEFAULT
ENV VERSION=$VERSION_DEFAULT

ENV GOPATH /go
ENV PATH ${GOPATH}/bin:$PATH
ENV GO111MODULE=on
ENV CGO_ENABLED 0

RUN echo "version: ${VERSION}"

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

COPY --from=builder "/app/build/migrator.linux.x64" /app/migrator
COPY --from=builder /app/config.example.toml /app/config.toml

WORKDIR /app
