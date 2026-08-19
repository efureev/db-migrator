#!/usr/bin/env bash
#
# Builds the migrator binary.
#
# Two defects in the version-1 script are fixed here by name, because both
# survived eight releases without anybody noticing:
#
#   1. The -X flags named "migrator/src/commands.version" while the module was
#      "github.com/efureev/db-migrator". The linker ignores an unknown symbol
#      silently, so every released binary printed "unknown (unknown)". The path
#      below is the real one, and internal/buildinfo/ldflags_test.go reads this
#      file and checks it against the package's actual import path.
#
#   2. The architecture suffix was the constant "x64" whatever GOARCH said, and
#      darwin/arm64 was never built at all — on the machine the tool was written
#      on, the release shipped a binary that could not run.

set -euo pipefail

APP_NAME=${APP_NAME:-migrator}
BUILD_DIR=${BUILD_DIR:-build}
LDFLAGS_PKG=${LDFLAGS_PKG:-github.com/efureev/db-migrator/v2/internal/buildinfo}

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo none)}
DATE=${DATE:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}

ldflags="-s -w \
  -X ${LDFLAGS_PKG}.version=${VERSION} \
  -X ${LDFLAGS_PKG}.commit=${COMMIT} \
  -X ${LDFLAGS_PKG}.date=${DATE}"

build_one() {
  local goos=$1 goarch=$2
  local out="${BUILD_DIR}/${APP_NAME}_${goos}_${goarch}"

  [ "$goos" = windows ] && out="${out}.exe"

  echo "  ${goos}/${goarch} -> ${out}"

  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/migrator
}

mkdir -p "$BUILD_DIR"

echo "migrator ${VERSION} (${COMMIT})"

if [ "${BUILD_ALL:-0}" = "1" ]; then
  # darwin/arm64 first, deliberately: it is the platform this is written on and
  # the one v1 never shipped.
  for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
    build_one "${target%/*}" "${target#*/}"
  done

  ( cd "$BUILD_DIR" && shasum -a 256 -- * > SHA256SUMS 2>/dev/null || sha256sum -- * > SHA256SUMS )
  echo "  checksums -> ${BUILD_DIR}/SHA256SUMS"
else
  build_one "$(go env GOOS)" "$(go env GOARCH)"
fi
