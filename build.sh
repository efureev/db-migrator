#!/usr/bin/env bash

BUILD_PATH="build"
APP_NAME="migrator"
VERSION_BUILD=$(git log --pretty="%h" -n1 HEAD)
VERSION_TAG=$(git describe --abbrev=0 --tags)
BUILD_TIME_LOCAL=$(date -u '+%Y-%m-%d_%H:%M:%S')

VERSION=${VERSION:-$VERSION_TAG}
BUILD_TIME=${BUILD_TIME:-$BUILD_TIME_LOCAL}

echo "Building options"
echo "- VERSION: $VERSION"
echo "- COMMIT: $VERSION_BUILD"
echo "- BUILD_TIME: $BUILD_TIME"
echo " "


for OS in darwin linux ;
# for OS in darwin ;
do
    for ARCH in amd64 ;
        do
            ARCHX=x86
            if [ $ARCH == "amd64" ]
            then
                ARCHX=x64
            fi
            echo "Building -> OS: $OS ARCH: $ARCH file: $APP_NAME.$OS.$ARCHX" ;

            GOOS=$OS GOARCH=$ARCH go build -ldflags="\
                -X 'migrator/src/commands.version=$VERSION_TAG' \
                -X 'migrator/src/commands.build=$VERSION_BUILD' \
                -X 'migrator/src/commands.buildTime=$BUILD_TIME' \
                " \
                 -o $BUILD_PATH/$APP_NAME.$OS.$ARCHX ;

        done
done
echo ""
