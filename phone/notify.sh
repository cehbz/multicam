#!/system/bin/sh
# Runs as root on the phone: runs rig.sh with an action and shows its output
# in the "rig" notification, which each run replaces. A start shows
# "Starting rig..." while it runs, and opens the console in Chrome once
# multicam runs. Nothing is posted when rig.sh ends on a signal, as a start
# does when a stop ends it; the stop posts its own.
# Usage: notify.sh start | stop | status
D=$(cd "$(dirname "$0")" && pwd)

# Posts the notification as the shell user: one posted as root is dropped.
post() {
	TITLE=$1 TEXT=$2 su 2000 -c 'cmd notification post -S bigtext -t "$TITLE" rig "$TEXT"' >/dev/null
}

# Opens url in Chrome, in the tab an earlier open used: Chrome reuses a tab
# for intents with the same application id.
open_console() {
	am start -a android.intent.action.VIEW -d "$1" \
		-e com.android.browser.application_id multicam com.android.chrome >/dev/null 2>&1
}

case "${1:-}" in
start) title="Start rig" ;;
stop) title="Stop rig" ;;
status) title="Rig status" ;;
*)
	echo "usage: notify.sh start | stop | status" >&2
	exit 2
	;;
esac
[ "$1" = start ] && post "$title" "Starting rig..."
# rig.sh's lines as they come, then its exit status as a last line.
out=$({
	sh "$D/rig.sh" "$1" 2>&1
	echo "rc=$?"
} | while IFS= read -r line; do
	case $line in
	("multicam: running"*) up=1 ;;
	("console: "*) [ "$1" = start ] && [ -n "$up" ] && open_console "${line#console: }" ;;
	esac
	echo "$line"
done)
rc=$(echo "$out" | tail -n 1)
[ "${rc#rc=}" -gt 128 ] && exit 0
post "$title" "$(echo "$out" | sed '$d')"
