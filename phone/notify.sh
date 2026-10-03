#!/system/bin/sh
# Runs as root on the phone: runs rig.sh with an action and shows its output
# in the "rig" notification, which each run replaces. A start shows
# "Starting rig..." while it runs. Nothing is posted when rig.sh ends on a
# signal, as a start does when a stop ends it; the stop posts its own.
# Usage: notify.sh start | stop | status
D=$(cd "$(dirname "$0")" && pwd)

# Posts the notification as the shell user: one posted as root is dropped.
post() {
	TITLE=$1 TEXT=$2 su 2000 -c 'cmd notification post -S bigtext -t "$TITLE" rig "$TEXT"' >/dev/null
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
out=$(sh "$D/rig.sh" "$1" 2>&1)
[ $? -gt 128 ] && exit 0
post "$title" "$out"
