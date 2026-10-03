#!/usr/bin/env bash
# Cross-build multicam and push it, the rig's config, the phone scripts
# (phone/*.sh to /data/local/tmp/mc) and the Termux:Widget tasks (to
# ~/.shortcuts/tasks in Termux's home) over adb, then restart the server through
# rig.sh so it runs detached from this session, with the console port
# forwarded to this Mac. With --foreground the server instead runs in this
# adb session as root and Ctrl-C stops it; the camera links and MediaMTX stay
# whatever rig.sh left them.
# Usage: scripts/phone-run.sh [--foreground]
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
bin=$repo/bin/multicam-android
remote=/data/local/tmp/multicam
mc=/data/local/tmp/mc
config=$mc/multicam.toml
termux_home=/data/data/com.termux/files/home
port=8080
foreground=0
[[ ${1:-} == --foreground ]] && foreground=1

(cd "$repo" && CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o "$bin" ./cmd/multicam) || exit 1
adb get-state >/dev/null || exit 1

# SIGTERM, up to 5 s for a clean exit, then SIGKILL; fails if it survives.
stop_remote() {
	adb shell "su -c 'pkill -x multicam
		for i in 1 2 3 4 5; do pidof multicam >/dev/null || exit 0; sleep 1; done
		pkill -9 -x multicam; sleep 1; ! pidof multicam >/dev/null'"
}

# The binary is renamed over the old one, so a running server keeps its
# inode and rig.sh still finds it by /proc.
adb push "$bin" "$remote.new" >/dev/null || exit 1
adb shell "mv $remote.new $remote && chmod 755 $remote" || exit 1
adb shell "mkdir -p $mc/shortcuts" || exit 1
adb push "$repo/multicam.phone.toml" "$config" >/dev/null || exit 1
adb push "$repo/phone/links.sh" "$repo/phone/udhcpc.sh" "$repo/phone/rig.sh" "$repo/phone/notify.sh" "$mc/" >/dev/null || exit 1
adb shell "chmod 755 $mc/links.sh $mc/udhcpc.sh $mc/rig.sh $mc/notify.sh" || exit 1
if [[ -f $repo/phone/links.conf ]]; then
	adb push "$repo/phone/links.conf" "$mc/" >/dev/null || exit 1
	adb shell "chmod 600 $mc/links.conf" || exit 1
fi

# Termux's home is private to its uid, so the tasks go in as root and are
# given back to that uid with the SELinux label its files carry.
owner=$(adb shell "su -c 'stat -c %u:%g $termux_home'" | tr -d '\r')
[[ $owner =~ ^[0-9]+:[0-9]+$ ]] || {
	echo "could not read the owner of $termux_home" >&2
	exit 1
}
adb push "$repo"/phone/shortcuts/tasks/* "$mc/shortcuts/" >/dev/null || exit 1
adb shell "su -c 'mkdir -p $termux_home/.shortcuts/tasks &&
	cp $mc/shortcuts/* $termux_home/.shortcuts/tasks/ &&
	chown -R $owner $termux_home/.shortcuts &&
	chmod 700 $termux_home/.shortcuts $termux_home/.shortcuts/tasks $termux_home/.shortcuts/tasks/* &&
	restorecon -RD $termux_home/.shortcuts'" || exit 1

adb forward "tcp:$port" "tcp:$port" >/dev/null || exit 1

if [[ $foreground == 0 ]]; then
	adb shell "su -c '$mc/rig.sh restart'"
	rc=$?
	echo "Console at http://localhost:$port/ (rig.sh on the phone stops it)"
	exit $rc
fi

cleaned=0
cleanup() {
	[[ $cleaned == 1 ]] && return
	cleaned=1
	if stop_remote; then
		echo "multicam is not running on the phone"
	else
		echo "warning: multicam is still running on the phone" >&2
		echo "  run: adb shell su -c 'pkill -9 -x multicam'" >&2
	fi
	adb forward --remove "tcp:$port" >/dev/null 2>&1
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if ! stop_remote; then
	echo "could not stop the previous multicam on the phone" >&2
	exit 1
fi
echo "Console at http://localhost:$port/ (Ctrl-C stops multicam on the phone)"

# -t gives the phone side a terminal, so Ctrl-C reaches multicam as SIGINT.
adb shell -t "su -c '$remote $config'"
rc=$?
echo "multicam exited $rc"
exit $rc
