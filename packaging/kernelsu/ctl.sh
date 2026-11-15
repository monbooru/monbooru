#!/system/bin/sh
# ctl.sh start|stop|restart|toggle|status|log
#
# The one place that starts and stops the server: service.sh calls it at boot,
# action.sh behind the manager's Action button calls it, and the module's
# WebUI calls it through the manager's shell bridge.
#
# MONBOORU_MODULE_DATA moves the data directory somewhere else, which is what
# lets these scripts be exercised off-device.

case "$0" in
*/*) MODDIR=${0%/*} ;;
*) MODDIR=. ;;
esac

DATA=${MONBOORU_MODULE_DATA:-/data/adb/monbooru}
BIN=$MODDIR/bin/monbooru
CFG=$DATA/monbooru.toml
PIDFILE=$DATA/monbooru.pid
LOG=$DATA/log/monbooru.log
MAX_LOG_BYTES=1048576

PATH=$DATA/bin:$PATH
export PATH

die() {
	echo "$*" >&2
	exit 1
}

# The first value of a key in the config; the server reads the same file, so
# this is the address it will listen on.
config_value() {
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$CFG" | head -n 1
}

running_pid() {
	[ -f "$PIDFILE" ] || return 1
	pid=$(cat "$PIDFILE" 2>/dev/null)
	[ -n "$pid" ] || return 1
	kill -0 "$pid" 2>/dev/null || return 1
	echo "$pid"
}

healthy() {
	"$BIN" healthcheck -config "$CFG" -timeout 1s >/dev/null 2>&1
}

start() {
	[ -x "$BIN" ] || die "missing $BIN"
	[ -f "$CFG" ] || die "missing $CFG"

	pid=$(running_pid) && {
		echo "already running (pid $pid)"
		return 0
	}
	rm -f "$PIDFILE"

	mkdir -p "$DATA/log" "$DATA/data" "$DATA/models"
	# The server serves a gallery it cannot open in a degraded mode, so make
	# the path first; a read-only or unmounted card is left to the log.
	gallery=$(config_value gallery_path)
	[ -n "$gallery" ] && mkdir -p "$gallery" 2>/dev/null

	if [ -f "$LOG" ] && [ "$(wc -c <"$LOG")" -gt "$MAX_LOG_BYTES" ]; then
		mv -f "$LOG" "$LOG.1"
	fi

	cd "$DATA" || die "cannot enter $DATA"
	nohup "$BIN" -config "$CFG" >>"$LOG" 2>&1 &
	echo $! >"$PIDFILE"

	i=0
	while [ "$i" -lt 20 ]; do
		if healthy; then
			echo "started (pid $(cat "$PIDFILE"))"
			return 0
		fi
		sleep 1
		i=$((i + 1))
	done

	echo "still not healthy after 20s; last log lines:" >&2
	tail -n 20 "$LOG" >&2 2>/dev/null
	return 1
}

stop() {
	pid=$(running_pid) || {
		rm -f "$PIDFILE"
		echo "not running"
		return 0
	}

	kill "$pid" 2>/dev/null
	i=0
	while [ "$i" -lt 10 ]; do
		kill -0 "$pid" 2>/dev/null || {
			rm -f "$PIDFILE"
			echo "stopped"
			return 0
		}
		sleep 1
		i=$((i + 1))
	done

	kill -9 "$pid" 2>/dev/null
	rm -f "$PIDFILE"
	echo "stopped (killed)"
}

status() {
	pid=$(running_pid)
	if [ -n "$pid" ]; then
		echo "running=1"
		echo "pid=$pid"
		if healthy; then echo "health=ok"; else echo "health=down"; fi
	else
		echo "running=0"
		echo "pid="
		echo "health=down"
	fi

	addr=$(config_value bind_address)
	[ -n "$addr" ] || addr="127.0.0.1:8455"
	host=${addr%:*}
	port=${addr##*:}
	# A wildcard bind is still reached on the phone itself through loopback.
	[ "$host" = "0.0.0.0" ] || [ -z "$host" ] && host=127.0.0.1
	echo "url=http://$host:$port"

	version=$(sed -n 's/^version=//p' "$MODDIR/module.prop" 2>/dev/null | head -n 1)
	echo "version=$version"
}

show_log() {
	if [ -f "$LOG" ]; then
		tail -n 200 "$LOG"
	else
		echo "(no log yet)"
	fi
}

case "${1:-status}" in
start) start ;;
stop) stop ;;
restart)
	stop
	start
	;;
toggle)
	if running_pid >/dev/null; then stop; else start; fi
	;;
status) status ;;
log) show_log ;;
*) die "usage: ctl.sh start|stop|restart|toggle|status|log" ;;
esac
