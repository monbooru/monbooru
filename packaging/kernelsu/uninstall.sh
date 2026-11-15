#!/system/bin/sh
# Uninstalling stops the server and keeps the library, the config and the log:
# they are the user's files, not the module's.

case "$0" in
*/*) MODDIR=${0%/*} ;;
*) MODDIR=. ;;
esac

DATA=${MONBOORU_MODULE_DATA:-/data/adb/monbooru}

sh "$MODDIR/ctl.sh" stop >/dev/null 2>&1
echo "monbooru stopped; $DATA is kept"
