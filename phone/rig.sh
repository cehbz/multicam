#!/system/bin/sh
# Runs as root on the phone: brings the rig up and down, each daemon
# detached from the shell that starts it so it outlives an adb session or a
# Termux:Widget tap.
# Usage: rig.sh start | stop [links] | restart | status
#   start    joins the cameras (links.sh), then starts MediaMTX and multicam
#            unless they run already; logs in mediamtx.log and multicam.log
#            here, pids in *.pid. Fails if a camera is not joined or a daemon
#            did not start.
#   stop     stops multicam, then MediaMTX (TERM, KILL after 5 s). The camera
#            links stay up; `stop links` ends their wpa_supplicants too.
#   restart  stops multicam and runs start.
#   status   what is running, each link's state and the console URL; succeeds
#            when the whole rig is up.
# Termux:Widget (from F-Droid) runs the scripts in ~/.shortcuts of Termux's
# home (/data/data/com.termux/files/home/.shortcuts, executable and owned by
# the Termux user) and shows their output. phone/shortcuts in the repo holds
# "Start rig", "Stop rig" and "Rig status", which call this script through
# su; scripts/phone-run.sh pushes them with this script.
BIN=/data/data/com.termux/files/usr/bin
D=$(cd "$(dirname "$0")" && pwd)
MULTICAM=/data/local/tmp/multicam
MEDIAMTX=$D/mtx/mediamtx
CONSOLE=http://localhost:8080/
SETSID=$(command -v setsid)

# Pids of the processes running exe, by name: a binary pushed over a running
# one keeps its name, and a scan of /proc/*/exe takes 15 s on this phone.
pids() {
	pidof "${1##*/}"
}

# Starts exe in dir with the remaining arguments unless it runs already,
# appending its output to $D/name.log and recording its pids in $D/name.pid.
start_daemon() {
	name=$1 exe=$2 dir=$3
	shift 3
	p=$(pids "$exe")
	if [ -z "$p" ]; then
		# The launching subshell drops its own stdio too: it lingers as the
		# daemon's parent on this phone and would hold an adb session open.
		(cd "$dir" && $SETSID nohup "$exe" "$@" >>"$D/$name.log" 2>&1 </dev/null &) >/dev/null 2>&1 </dev/null
		sleep 1
		p=$(pids "$exe")
	fi
	if [ -z "$p" ]; then
		rm -f "$D/$name.pid"
		echo "$name: did not start (see $D/$name.log)"
		return 1
	fi
	echo "$p" >"$D/$name.pid"
	echo "$name: running ($(echo $p))"
}

# SIGTERM, up to 5 s for a clean exit, then SIGKILL; fails if it survives.
stop_daemon() {
	name=$1 exe=$2
	p=$(pids "$exe")
	rm -f "$D/$name.pid"
	if [ -z "$p" ]; then
		echo "$name: not running"
		return 0
	fi
	kill $p
	for i in 1 2 3 4 5; do
		[ -z "$(pids "$exe")" ] && break
		sleep 1
	done
	if [ -n "$(pids "$exe")" ]; then
		kill -9 $p
		sleep 1
	fi
	if [ -n "$(pids "$exe")" ]; then
		echo "$name: still running ($(echo $p))"
		return 1
	fi
	echo "$name: stopped"
}

report() {
	p=$(pids "$2")
	if [ -z "$p" ]; then
		echo "$1: not running"
		return 1
	fi
	echo "$1: running ($(echo $p))"
}

cameras() { grep -v -E '^[[:space:]]*(#|$)' "$D/links.conf"; }
cli() {
	iface=$1
	shift
	$BIN/wpa_cli -p "$D/ctrl" -i "$iface" "$@" 2>/dev/null
}
state() { cli "$1" status | sed -n 's/^wpa_state=//p'; }
addr() { ip -4 addr show dev "$1" 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p'; }

# Prints each camera link's state; fails unless every camera is joined.
links_status() {
	cameras | {
		down=0
		while read -r IF SSID PSK; do
			s=$(state "$IF")
			if [ "$s" = COMPLETED ]; then
				echo "$IF: joined $SSID as $(addr "$IF")"
			else
				echo "$IF: $SSID not joined (${s:-down})"
				down=1
			fi
		done
		[ $down = 0 ]
	}
}

# Ends each camera's wpa_supplicant and drops its address; the interfaces
# stay, links.sh reuses them.
stop_links() {
	cameras | while read -r IF SSID PSK; do
		cli "$IF" terminate >/dev/null
		ip addr flush dev "$IF" 2>/dev/null
		echo "$IF: left $SSID"
	done
}

start() {
	rc=0
	sh "$D/links.sh" || rc=1
	links_status >/dev/null || rc=1
	start_daemon mediamtx "$MEDIAMTX" "$D/mtx" || rc=1
	start_daemon multicam "$MULTICAM" "$D" "$D/multicam.toml" || rc=1
	echo "console: $CONSOLE"
	return $rc
}

stop() {
	rc=0
	stop_daemon multicam "$MULTICAM" || rc=1
	stop_daemon mediamtx "$MEDIAMTX" || rc=1
	[ "$1" = links ] && stop_links
	return $rc
}

status() {
	rc=0
	report multicam "$MULTICAM" || rc=1
	report mediamtx "$MEDIAMTX" || rc=1
	links_status || rc=1
	echo "console: $CONSOLE"
	return $rc
}

case "${1:-}" in
start) start ;;
stop) stop "${2:-}" ;;
restart) stop_daemon multicam "$MULTICAM" && start ;;
status) status ;;
*)
	echo "usage: rig.sh start | stop [links] | restart | status" >&2
	exit 2
	;;
esac
