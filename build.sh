#!/usr/bin/env bash

BUILD_FOR_DOCKER=${BUILD_FOR_DOCKER}
BUILD_PATH=${BUILD_PATH:-"build"}
APP_NAME=${APP_NAME:-'migrator'}
VERSION_BUILD=$(git log --pretty="%h" -n1 HEAD)
VERSION_TAG=$(git describe --abbrev=0 --tags)
BUILD_TIME_LOCAL=$(date -u '+%Y-%m-%d_%H:%M:%S')

VERSION=${VERSION:-$VERSION_TAG}
BUILD_TIME=${BUILD_TIME:-$BUILD_TIME_LOCAL}

# This list is based on:
# https://github.com/golang/go/blob/master/src/go/build/syslist.go
gooses=(
    darwin linux #windows
)
gooses=($(printf -- '%s\n' "${gooses[@]}" | sort))

# This list is based on:
# https://github.com/golang/go/blob/master/src/go/build/syslist.go
goarches=(
    amd64 arm64
)
goarches=($(printf -- '%s\n' "${goarches[@]}" | sort))

echo "Building options"
echo "- VERSION: $VERSION"
echo "- COMMIT: $VERSION_BUILD"
echo "- BUILD_TIME: $BUILD_TIME"
echo " "

BUILDING_FLAGS="\
     -X 'migrator/src/commands.version=$VERSION_TAG' \
     -X 'migrator/src/commands.build=$VERSION_BUILD' \
     -X 'migrator/src/commands.buildTime=$BUILD_TIME' \
"

if [ "$BUILD_FOR_DOCKER" == '1' ]; then
  BUILDING_FLAGS="$BUILDING_FLAGS -s -w"
fi

for goos in "${gooses[@]}"
do
    for goarch in "${goarches[@]}"
    do
#        GOOS="$goos" GOARCH="$goarch" go build -o /dev/null main.go >out.log 2>err.log
        CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -ldflags="$BUILDING_FLAGS" -o $BUILD_PATH/$APP_NAME.$goos.$goarch main.go >out.log 2>err.log
        if [ $? -eq 0 ]
        then
            :
        else
            if grep -qe '^cmd/go: unsupported GOOS/GOARCH pair' err.log
            then
                :
            else
                mv err.log $goos-$goarch.err.log
            fi
        fi
        if [ -s out.log ]
        then
            mv out.log $goos-$goarch.out.log
        fi
    done
done
rm -f out.log err.log
echo ""
