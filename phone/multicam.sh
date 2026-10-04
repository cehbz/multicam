#!/system/bin/sh
# Supervises the multicam server on the phone. Installed as
# /data/adb/service.d/multicam.sh, which Magisk runs as root once at boot;
# scripts/phone-run.sh runs it again after each deploy.
#
# Runs the server with its config in its working directory, output appended
# to the log, and starts it again 1 s after it exits. After five exits in a
# row, each within 10 s of its start, it posts a notification and stops; a
# longer run clears the count. While the hold file exists the server isn't
# started, and the count is cleared when it goes.
#
# One supervisor runs: a second one signals the first (USR1 clears its crash
# count) and exits 0. TERM stops the server and the supervisor.
# The MC_* variables set the paths and timings; each defaults to the phone's.
SERVER=${MC_SERVER:-/data/local/tmp/multicam}
CONFIG=${MC_CONFIG:-/data/local/tmp/mc/multicam.toml}
DIR=${MC_DIR:-/data/local/tmp/mc}
LOG=${MC_LOG:-/data/local/tmp/mc/multicam.log}
HOLD=${MC_HOLD:-/data/local/tmp/mc/hold}
PIDFILE=${MC_PIDFILE:-/data/local/tmp/mc/supervisor.pid}
QUICK=${MC_QUICK:-10}
MAX=${MC_MAX:-5}
DELAY=${MC_RESTART_DELAY:-1}
POLL=${MC_POLL:-1}

# The pid file outlives a reboot, so a pid is the supervisor's only if its
# command line names this script.
if [ -s "$PIDFILE" ]; then
	old=$(cat "$PIDFILE")
	case $(ps -o args= -p "$old" 2>/dev/null) in
	*multicam.sh*)
		kill -USR1 "$old"
		exit 0
		;;
	esac
fi
echo $$ >"$PIDFILE"

pid=
count=0
log() { echo "supervisor: $*" >>"$LOG"; }
trap 'count=0' USR1
trap '[ -n "$pid" ] && kill "$pid" 2>/dev/null; exit 0' TERM INT
trap '[ "$(cat "$PIDFILE" 2>/dev/null)" = $$ ] && rm -f "$PIDFILE"' EXIT

# Posts as the shell user: a notification posted as root is dropped. A post
# with the same tag replaces the last.
post() {
	TITLE=$1 TEXT=$2 su 2000 -c 'cmd notification post -S bigtext -t "$TITLE" multicam "$TEXT"' >/dev/null 2>&1
}

while :; do
	if [ -e "$HOLD" ]; then
		while [ -e "$HOLD" ]; do sleep "$POLL"; done
		count=0
	fi
	started=$(date +%s)
	(cd "$DIR" && exec "$SERVER" "$CONFIG") >>"$LOG" 2>&1 </dev/null &
	pid=$!
	# A trap ends wait early; wait again until the server is gone.
	while :; do
		wait "$pid"
		kill -0 "$pid" 2>/dev/null || break
	done
	ran=$(($(date +%s) - started))
	if [ "$ran" -lt "$QUICK" ]; then count=$((count + 1)); else count=0; fi
	log "server exited after ${ran}s ($count quick in a row)"
	if [ "$count" -ge "$MAX" ]; then
		log "crash-looping, not restarting"
		post "multicam" "The server is crash-looping and was not restarted. See $LOG"
		exit 1
	fi
	sleep "$DELAY"
done
