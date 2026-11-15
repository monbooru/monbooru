#!/bin/sh
# packaging/build-kernelsu.sh
#
# Packs the arm64 server and the module's scripts into the KernelSU module zip
# under dist/kernelsu/, next to the update.json the manager reads. The zip's
# own root is the module root: module.prop sits at the top, with no directory
# above it and nothing under system/.
set -eu

. ./packaging/release-env.sh
mkdir -p dist stage

command -v zip >/dev/null || {
	echo "zip is needed to pack the module" >&2
	exit 1
}

# versionCode is what the manager compares: 1.24.0 -> 12400.
ver=${VERSION#v}
major=${ver%%.*}
rest=${ver#*.}
minor=${rest%%.*}
patch=${rest#*.}
case "${major}${minor}${patch}" in
'' | *[!0-9]*)
	echo "cannot read a versionCode out of $VERSION" >&2
	exit 1
	;;
esac
code=$((major * 10000 + minor * 100 + patch))

name="monbooru_${ver}_kernelsu_arm64"
dir="stage/$name"
out=dist/kernelsu
zipname="$name.zip"
update_base="$REPO/releases/latest/download"

rm -rf "$dir" "$out"
mkdir -p "$dir/bin" "$dir/webroot" "$out"

# The server profile: no desktop shell, no tray, so the module can run it from
# a service script without a display.
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
	-ldflags="-s -w $LDFLAGS -X '$MOD.Package=kernelsu'" \
	-o "$dir/bin/monbooru" ./cmd/monbooru

cp packaging/kernelsu/customize.sh packaging/kernelsu/ctl.sh \
	packaging/kernelsu/service.sh packaging/kernelsu/action.sh \
	packaging/kernelsu/uninstall.sh packaging/kernelsu/monbooru.toml \
	"$dir/"
cp packaging/kernelsu/webroot/index.html packaging/kernelsu/webroot/style.css \
	packaging/kernelsu/webroot/app.js "$dir/webroot/"
# The manager's WebView runs as the app's own uid, so the page and its assets
# have to be world-readable and the scripts executable whatever the checkout's
# umask was.
chmod 0644 "$dir"/*.toml "$dir"/webroot/*
chmod 0755 "$dir/bin/monbooru" "$dir"/*.sh

sed -e "s|@VERSION@|$VERSION|g" \
	-e "s|@VERSIONCODE@|$code|g" \
	-e "s|@UPDATEJSON@|$update_base/kernelsu-update.json|g" \
	packaging/kernelsu/module.prop.in >"$dir/module.prop"
chmod 0644 "$dir/module.prop"
grep -qx "id=$APP" "$dir/module.prop" || {
	echo "module.prop lost its id" >&2
	exit 1
}
grep -qx "versionCode=$code" "$dir/module.prop" || {
	echo "module.prop did not take the versionCode" >&2
	exit 1
}

# The manager shows this next to the update it offers. The markdown links and
# the inline HTML in the changelog are flattened to text first, then escaped
# once for JSON: printf takes the result as-is, so nothing unescapes it again.
changelog=$(awk -v v="$VERSION" 'index($0, "## [" v "]") == 1 { on = 1; next } on && /^## / { exit } on' CHANGELOG.md |
	sed -e 's/<[^>]*>//g' -e 's/\[\([^]]*\)\](\([^)]*\))/\1/g' |
	sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' |
	grep -v '^$' |
	tr '\n' ' ')
changelog=$(printf '%s' "$changelog" | sed -e 's/[[:space:]]\{2,\}/ /g' -e 's/^ //' -e 's/ $//')
[ -n "$changelog" ] || changelog="See the release notes."
if [ "$(printf '%s' "$changelog" | wc -c)" -gt 500 ]; then
	changelog=$(printf '%s' "$changelog" | cut -c1-500 | sed -e 's/ [^ ]*$//' -e 's/$/ .../')
fi
changelog=$(printf '%s' "$changelog" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')

{
	printf '{\n'
	printf '  "version": "%s",\n' "$VERSION"
	printf '  "versionCode": %s,\n' "$code"
	printf '  "zipUrl": "%s",\n' "$update_base/$zipname"
	printf '  "changelog": "%s"\n' "$changelog"
	printf '}\n'
} >"$out/kernelsu-update.json"
grep -q '"zipUrl"' "$out/kernelsu-update.json" || {
	echo "update.json lost its zipUrl" >&2
	exit 1
}

if grep -q '@[A-Z][A-Z]*@' "$dir/module.prop" "$out/kernelsu-update.json"; then
	echo "a template marker was left unstamped" >&2
	exit 1
fi

(cd "$dir" && zip -qr "../../$out/$zipname" .)

list=$(unzip -Z1 "$out/$zipname")
for entry in module.prop bin/monbooru webroot/index.html webroot/app.js \
	customize.sh ctl.sh service.sh action.sh uninstall.sh monbooru.toml; do
	echo "$list" | grep -qx "$entry" || {
		echo "the zip has no $entry at its root" >&2
		exit 1
	}
done
if echo "$list" | grep -q '^system/'; then
	echo "the zip carries a system/ overlay; this module needs none" >&2
	exit 1
fi

ls -la "$out"
