#!/system/bin/sh
# Late-start service: wait for the parts the library needs, then hand the
# server to ctl.sh, which owns the process from there on.

case "$0" in
*/*) MODDIR=${0%/*} ;;
*) MODDIR=. ;;
esac

[ -f "$MODDIR/disable" ] && exit 0

wait_for() {
	seconds=$1
	shift
	i=0
	while [ "$i" -lt "$seconds" ]; do
		"$@" && return 0
		sleep 2
		i=$((i + 2))
	done
	return 1
}

booted() { [ "$(getprop sys.boot_completed 2>/dev/null)" = "1" ]; }
storage_ready() { [ -d /sdcard/Pictures ]; }

# 127.0.0.1 comes up with the module, but a gallery on /sdcard and the boot
# flag do not, so wait for them; a slow boot still gets a server.
wait_for 300 booted || echo "service.sh: boot flag never set, starting anyway" >&2
wait_for 120 storage_ready || echo "service.sh: /sdcard not mounted, starting anyway" >&2

sh "$MODDIR/ctl.sh" start
