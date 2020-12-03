#!/usr/bin/env bash

BUILD_PATH="build"
APP_NAME="migrator"

for OS in darwin linux ;
do
    for ARCH in amd64 ;
        do
            ARCHX=x86
            if [ $ARCH == "amd64" ]
            then
                ARCHX=x64
            fi
            echo "Building -> OS: $OS ARCH: $ARCH file: $APP_NAME.$OS.$ARCHX" ;
            GOOS=$OS GOARCH=$ARCH go build -o $BUILD_PATH/$APP_NAME.$OS.$ARCHX ;
        done
done
echo ""
