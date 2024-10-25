#!/usr/bin/env bash

set -euxo pipefail

TARGET=${1:-${TARGET:-'local'}};
APP_NAME=${APP_NAME:-'migrate'};
BUILD_FOR_DOCKER=${BUILD_FOR_DOCKER:-'0'};
allow=("local" "gh")
targetFound=0

for i in "${allow[@]}"
do
  if [[ $i == "$TARGET" ]]
  then
    targetFound=1
  fi
done

if [ "$targetFound" == 0 ]; then
  echo "Not found a Target"
  exit;
fi

echo "Building for $TARGET..."

####

VERSION_TAG=${VERSION_TAG:-"-"}
VERSION_BUILD="-"
BUILD_TIME_LOCAL=$(date -u '+%Y-%m-%d_%H:%M:%S')
BUILD_TIME=${BUILD_TIME:-$BUILD_TIME_LOCAL}

#
if [[ "$TARGET" == 'local' ]]; then

  VERSION_TAG=$(git describe --abbrev=0 --tags)
  VERSION_BUILD=$(git log --pretty="%h" -n1 HEAD)

elif [[ "$TARGET" == 'gh' ]]; then

  env

#  VERSION_BUILD=$(git log --pretty="%h" -n1 HEAD)

  if [ "$BUILDPLATFORM" != "$TARGETPLATFORM" ]; then
      echo "Cross-compiling to $TARGETPLATFORM"
      # $TARGETPLATFORM is something like:
      #   linux/amd64
      #   linux/arm64
      #   linux/arm/v8
      target_platform=(${TARGETPLATFORM//\// })
      export GOOS=${target_platform[0]}
      export GOARCH=${target_platform[1]}
      if [ "${#target_platform[@]}" -gt 2 ]; then
          export GOARM=${target_platform[2]//v}
      fi
  else
      echo "Compiling to $TARGETPLATFORM"
  fi
fi

BUILDING_FLAGS="\
     -X 'migrator/src/commands.version=$VERSION_TAG' \
     -X 'migrator/src/commands.build=$VERSION_BUILD' \
     -X 'migrator/src/commands.buildTime=$BUILD_TIME' \
"

if [ "$BUILD_FOR_DOCKER" == '1' ]; then
  BUILDING_FLAGS="$BUILDING_FLAGS -s -w"
fi

CGO_ENABLED=0 go build -ldflags="$BUILDING_FLAGS" -o "$APP_NAME"