#!/system/bin/sh
# Runs from the KernelSU installer once the zip is unpacked into $MODPATH.
#
# Nothing here touches /system: the module carries its own binary and keeps
# everything else under /data/adb/monbooru, which is also why it needs no
# metamodule and no skip_mount.

if [ "$ARCH" != "arm64" ]; then
	abort "! monbooru ships arm64 only; this device reports $ARCH"
fi

DATA=/data/adb/monbooru
ui_print "- Installing monbooru @VERSION@ for arm64"

mkdir -p "$DATA/data" "$DATA/log" "$DATA/models" "$DATA/bin"

if [ -f "$DATA/monbooru.toml" ]; then
	ui_print "- Keeping the existing $DATA/monbooru.toml"
else
	cp -f "$MODPATH/monbooru.toml" "$DATA/monbooru.toml"
	ui_print "- Wrote a starting config to $DATA/monbooru.toml"
fi

# The gallery is on the storage the user can see. Creating it now keeps the
# first start out of the degraded mode a missing path would put it in.
mkdir -p /sdcard/Pictures/monbooru 2>/dev/null ||
	ui_print "! Could not create /sdcard/Pictures/monbooru; unlocking storage will let the first start make it"

set_perm_recursive "$MODPATH" 0 0 0755 0644
set_perm "$MODPATH/bin/monbooru" 0 0 0755
for script in customize.sh ctl.sh service.sh action.sh uninstall.sh; do
	set_perm "$MODPATH/$script" 0 0 0755
done

ui_print "- Start it from the module's page, or tap Action to toggle it"
ui_print "- It listens on 127.0.0.1:8455; set server.bind_address for LAN access"
ui_print "- ffmpeg and ffprobe are picked up from $DATA/bin if you put them there"
