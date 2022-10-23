#ARG APP='App'
#ARG BUILD_PATH='build'
#ARG VERSION_DEFAULT=0.0.0
#ARG BUILD_TIME='-'
FROM golang AS builder
ARG APP
ARG BUILD_PATH
ARG VERSION_DEFAULT
ARG BUILD_TIME='-'

ENV VERSION=$VERSION_DEFAULT
ENV BUILD_TIME=$BUILD_TIME
ENV APP_NAME=$APP
ENV BUILD_PATH=$BUILD_PATH

ENV GOPATH /go
ENV PATH ${GOPATH}/bin:$PATH
ENV GO111MODULE=on
ENV CGO_ENABLED 0

RUN echo "app: $APP_NAME"
RUN echo "BUILD_PATH: ${BUILD_PATH}"
RUN echo "version: ${VERSION}"
RUN echo "build time: ${BUILD_TIME}"

WORKDIR /app
COPY go.mod go.sum ./

RUN go mod download

COPY . ./

RUN ls -la "/app"

RUN make build-docker

FROM alpine
ARG APP
ARG BUILD_PATH
ARG VERSION_DEFAULT

LABEL maintainer="Eugene Fureev <fureev@gmail.com>"
LABEL author="Eugene Fureev <fureev@gmail.com>"
LABEL version="${VERSION_DEFAULT}"

RUN echo "BUILD_PATH: $BUILD_PATH"
RUN echo "app: $APP"
RUN echo "BUILD_PATH: ${BUILD_PATH}"
RUN echo "VERSION_DEFAULT: $VERSION_DEFAULT"

RUN apk add --no-cache ca-certificates

COPY --from=builder "/app/${BUILD_PATH}/${APP}.linux.x64" /usr/local/bin/${APP}
COPY --from=builder /app/config.example.toml /app/config.example.toml

RUN ln -s /usr/local/bin/migrator /app/migrator

WORKDIR /app

#ENTRYPOINT ["/app/migrator"]

#CMD ["--help"]
