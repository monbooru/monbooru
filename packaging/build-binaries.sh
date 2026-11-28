#!/bin/sh
# packaging/build-binaries.sh <lite|bundled>
#
# Assembles the downloadable archive for GOOS/GOARCH into dist/, from the
# tools fetch-tools.sh left: lite is monbooru and ffmpeg, bundled adds
# the monworker beside them.
set -eu

shape=${1:?usage: build-binaries.sh lite|bundled}
. ./packaging/release-env.sh
mkdir -p dist stage

build() {
  os=$1
  arch=$2

  exe=monbooru
  pkg=tarball
  gui=""
  if [ "$os" = windows ]; then
    exe=monbooru.exe
    pkg=zip
    gui="-H windowsgui"
  fi

  name="monbooru_${VERSION#v}_portable_${shape}_${os}_${arch}"
  dir="stage/$name"
  rm -rf "$dir"
  mkdir -p "$dir"

  compile "$pkg" "$dir/$exe"
  cp -r tools/* "$dir/"
  if [ "$shape" = bundled ]; then
    cp -r worker/* "$dir/"
  fi

  cp LICENSE README.md "$dir/"
  if [ "$os" = windows ]; then
    cp packaging/icons/monbooru.ico "$dir/"
    rm -rf "$dir-setup"
    cp -r "$dir" "$dir-setup"
    compile installer "$dir-setup/$exe"
  fi

  : > "$dir/monbooru.toml"

  if [ "$os" = windows ]; then
    (cd stage && zip -qr "../dist/$name.zip" "$name")
  else
    tar -czf "dist/$name.tar.gz" -C stage "$name"
  fi
}

compile() {
  ldflags="-s -w $gui $LDFLAGS -X '$MOD.Package=$1' -X 'main.defaultDesktop=true'"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags="$ldflags" -o "$2" ./cmd/monbooru
}

case "$shape" in
lite | bundled) ;;
*)
  echo "unknown shape $shape" >&2
  exit 2
  ;;
esac
build "${GOOS:?GOOS must name the target}" "${GOARCH:?GOARCH must name the target}"
ls -la dist
