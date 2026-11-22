#!/bin/sh
# Builds the desktop bundled AppImage for $GOARCH into dist/, from the
# tools fetch-tools.sh left.
set -eu

. ./packaging/release-env.sh
. ./packaging/appimage-core.sh

appimage_begin

CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w $LDFLAGS -X '$MOD.Package=appimage' -X 'main.defaultDesktop=true'" \
  -o "$app/usr/bin/monbooru" ./cmd/monbooru

# The worker and its ONNX Runtime sit beside monbooru, where each looks.
cp tools/ffmpeg tools/ffprobe worker/monworker worker/libonnxruntime.so "$app/usr/bin/"
cp -r tools/licenses worker/licenses "$app/usr/share/"

appimage_finish
