#!/bin/sh
# sh build.sh           build ./rndr for this machine
# sh build.sh release   cross-compile static binaries for every platform into dist/
set -e
cd "$(dirname "$0")"
export CGO_ENABLED=0
if [ "$1" != release ]; then
    go build -trimpath -ldflags='-s -w' -o rndr .
    echo "built $(pwd)/rndr"
    exit 0
fi
rm -rf dist
mkdir dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 linux/arm linux/386 freebsd/amd64 windows/amd64 windows/arm64; do
    os=${target%/*}
    arch=${target#*/}
    name=rndr-$os-$arch
    [ "$arch" = arm ] && name=rndr-$os-armv7
    [ "$os" = windows ] && name=$name.exe
    GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -ldflags='-s -w' -o "dist/$name" .
    echo "dist/$name"
done
