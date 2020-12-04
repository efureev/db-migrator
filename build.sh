#!/usr/bin/env bash

BUILD_PATH="build"
APP_NAME="migrator"
VERSION_BUILD=$(git log --pretty="%h" -n1 HEAD)
VERSION_TAG=$(git describe --tags)

# for OS in darwin linux ;
for OS in darwin ;
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
                -X 'migrator/src/commands.buildTime=$(date)' \
                " \
                 -o $BUILD_PATH/$APP_NAME.$OS.$ARCHX ;
            
        done
done
echo ""
