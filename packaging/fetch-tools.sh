#!/bin/sh
# Downloads the bundled tools for one target: ffmpeg into tools/, which
# every shape ships, and the monworker archive into worker/, which the
# bundled shape adds. MONWORKER_ARCHIVE names a local archive to take
# instead of the pinned release, for a worker built from its checkout.
set -eu

case "${GOOS}/${GOARCH}" in
  linux/amd64)   ff_arch=linux64;    ff_sha=$FFMPEG_SHA256_LINUX64;    mw_sha=$MONWORKER_SHA256_LINUX_AMD64 ;;
  linux/arm64)   ff_arch=linuxarm64; ff_sha=$FFMPEG_SHA256_LINUXARM64; mw_sha=$MONWORKER_SHA256_LINUX_ARM64 ;;
  windows/amd64) ff_arch=win64;      ff_sha=$FFMPEG_SHA256_WIN64;      mw_sha=$MONWORKER_SHA256_WINDOWS_AMD64 ;;
  *) echo "no bundled tools published for ${GOOS}/${GOARCH}" >&2; exit 1 ;;
esac

verify() {
  echo "$2  $1" | sha256sum -c -
}

rm -rf tools worker /tmp/ff /tmp/mw
mkdir -p tools/licenses/ffmpeg worker /tmp/ff /tmp/mw
ff_base="https://github.com/monbooru/ffmpeg-builds/releases/download/${FFMPEG_BUILD}"

if [ "$GOOS" = windows ]; then
  curl -fsSL -o /tmp/ffmpeg.zip "${ff_base}/${FFMPEG_NAME}-${ff_arch}.zip"
  verify /tmp/ffmpeg.zip "$ff_sha"
  unzip -q /tmp/ffmpeg.zip -d /tmp/ff
  ff_dir="/tmp/ff/${FFMPEG_NAME}-${ff_arch}"
  cp "$ff_dir/bin/ffmpeg.exe" "$ff_dir/bin/ffprobe.exe" tools/
else
  curl -fsSL -o /tmp/ffmpeg.tar.xz "${ff_base}/${FFMPEG_NAME}-${ff_arch}.tar.xz"
  verify /tmp/ffmpeg.tar.xz "$ff_sha"
  tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ff
  ff_dir="/tmp/ff/${FFMPEG_NAME}-${ff_arch}"
  cp "$ff_dir/bin/ffmpeg" "$ff_dir/bin/ffprobe" tools/
fi
cp "$ff_dir"/licenses/* tools/licenses/ffmpeg/

ext=tar.gz
[ "$GOOS" = windows ] && ext=zip
name="monworker_${MONWORKER_VERSION}_${GOOS}_${GOARCH}"
if [ -n "${MONWORKER_ARCHIVE:-}" ]; then
  archive=$MONWORKER_ARCHIVE
else
  [ -n "$mw_sha" ] || {
    echo "monworker $MONWORKER_VERSION has no sum pinned for ${GOOS}/${GOARCH} in packaging/pins.env" >&2
    exit 1
  }
  archive=/tmp/monworker.$ext
  curl -fsSL -o "$archive" "https://github.com/monbooru/monworker/releases/download/v${MONWORKER_VERSION}/${name}.${ext}"
  verify "$archive" "$mw_sha"
fi
if [ "$ext" = zip ]; then
  unzip -q "$archive" -d /tmp/mw
else
  tar -xzf "$archive" -C /tmp/mw
fi
# The archive's README and empty models folder are the worker's own; its
# licence joins the others.
mw_dir=$(find /tmp/mw -mindepth 1 -maxdepth 1 -type d -name 'monworker_*' | head -1)
[ -n "$mw_dir" ] || { echo "no monworker_* folder in $archive" >&2; exit 1; }
mkdir -p worker/licenses/monworker
cp "$mw_dir/LICENSE" worker/licenses/monworker/
rm -rf "$mw_dir/README.md" "$mw_dir/LICENSE" "$mw_dir/models"
cp -r "$mw_dir"/. worker/

ls -laR tools worker
