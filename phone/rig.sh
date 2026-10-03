#!/system/bin/sh
# Runs as root on the phone: brings the rig up and down, each daemon
# detached from the shell that starts it so it outlives an adb session or a
# Termux:Widget tap.
# Usage: rig.sh start | stop [links] | restart | status
#   start    starts MediaMTX and multicam unless they run already, then joins
#            the cameras (links.sh); logs in mediamtx.log and multicam.log
#            here, pids in *.pid. Fails if a camera is not joined or a daemon
#            did not start, and is refused while a start or a stop runs.
#   stop     ends a running start, then stops multicam, then MediaMTX (TERM,
#            KILL after 5 s). The camera links stay up; `stop links` ends their
#            wpa_supplicants too. Refused while a stop runs, and says "nothing
#            to stop" when nothing runs.
#   restart  stops multicam and runs start.
# rig.pid holds the pid of the start or stop running.
#   status   what is running, each link's state and the console URL; succeeds
#            when the whole rig is up.
# Termux:Widget runs the scripts in ~/.shortcuts/tasks of Termux's home
# (/data/data/com.termux/files/home/.shortcuts/tasks, executable and owned by
# the Termux user) in the background. phone/shortcuts/tasks in the repo holds
# "Start rig", "Stop rig" and "Rig status", which run notify.sh through su;
# scripts/phone-run.sh pushes them with this script.
BIN=/data/data/com.termux/files/usr/bin
D=$(cd "$(dirname "$0")" && pwd)
MULTICAM=/data/local/tmp/multicam
OWNER=$D/rig.pid
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

# The kind of rig.sh action pid is running, start or stop; nothing when pid
# is gone or runs something else. restart is a start.
kind() {
	case $(ps -o args= -p "$1" 2>/dev/null) in
	*"rig.sh start"* | *"rig.sh restart"*) echo start ;;
	*"rig.sh stop"*) echo stop ;;
	esac
}

# The kind and pid of the other action running ("start 123"), from rig.pid.
owner() {
	p=$(cat "$OWNER" 2>/dev/null)
	[ -n "$p" ] && [ "$p" != $$ ] || return
	k=$(kind "$p")
	[ -n "$k" ] && echo "$k $p"
}

claim() { echo $$ >"$OWNER"; }
release() { [ "$(cat "$OWNER" 2>/dev/null)" = $$ ] && rm -f "$OWNER"; }

# Prints why a start can't run now and succeeds; fails when it can.
start_refused() {
	o=$(owner)
	case $o in
	start\ *) echo "start: already running (pid ${o#start })" ;;
	stop\ *) echo "start: a stop is running (pid ${o#stop })" ;;
	*) return 1 ;;
	esac
}

# pid and its descendants, parents first.
tree() {
	echo "$1"
	for c in $(pgrep -P "$1"); do
		tree "$c"
	done
}

# Ends the start running as pid and what it launched: TERM, KILL after 5 s.
end_start() {
	t=$(tree "$1")
	kill $t 2>/dev/null
	for i in 1 2 3 4 5; do
		kill -0 "$1" 2>/dev/null || break
		sleep 1
	done
	kill -0 "$1" 2>/dev/null && kill -9 $t 2>/dev/null
	echo "start: ended (pid $1)"
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

# Succeeds when a camera link has a wpa_supplicant.
links_up() {
	cameras | {
		while read -r IF SSID PSK; do
			[ -n "$(state "$IF")" ] && exit 0
		done
		exit 1
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
	start_refused && return 1
	claim
	rc=0
	start_daemon mediamtx "$MEDIAMTX" "$D/mtx" || rc=1
	start_daemon multicam "$MULTICAM" "$D" "$D/multicam.toml" || rc=1
	echo "console: $CONSOLE"
	sh "$D/links.sh" || rc=1
	links_status >/dev/null || rc=1
	release
	return $rc
}

stop() {
	o=$(owner)
	case $o in
	stop\ *)
		echo "stop: already running (pid ${o#stop })"
		return 1
		;;
	esac
	claim
	busy=0
	if [ -n "$o" ]; then
		end_start "${o#start }"
		busy=1
	fi
	[ -n "$(pids "$MULTICAM")$(pids "$MEDIAMTX")" ] && busy=1
	[ "$1" = links ] && links_up && busy=1
	if [ $busy = 0 ]; then
		release
		echo "nothing to stop"
		return 1
	fi
	rc=0
	stop_daemon multicam "$MULTICAM" || rc=1
	stop_daemon mediamtx "$MEDIAMTX" || rc=1
	[ "$1" = links ] && stop_links
	release
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
restart)
	start_refused && exit 1
	stop_daemon multicam "$MULTICAM" && start
	;;
status) status ;;
*)
	echo "usage: rig.sh start | stop [links] | restart | status" >&2
	exit 2
	;;
esac
