#!/system/bin/sh
# The manager runs this for its Action button and shows what it prints. It
# toggles the same process the module's page drives.

case "$0" in
*/*) MODDIR=${0%/*} ;;
*) MODDIR=. ;;
esac

sh "$MODDIR/ctl.sh" toggle
echo
sh "$MODDIR/ctl.sh" status
