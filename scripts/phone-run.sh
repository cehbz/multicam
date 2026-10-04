#!/usr/bin/env bash
# Cross-build multicam and push it, its config, wpa_supplicant with its
# libraries (bin/wifi) and the supervisor (phone/multicam.sh) to the phone over
# adb, install the supervisor in Magisk's service.d, make sure it runs, and end
# the running server with SIGTERM so the supervisor starts the new one. The
# console port is forwarded to this Mac.
# With --foreground the supervisor holds off (a hold file), the new server runs
# in this adb session as root, Ctrl-C stops it, and on exit the hold file is
# removed so the supervisor takes over.
# Usage: scripts/phone-run.sh [--foreground]
set -uo pipefail

export ANDROID_SERIAL=8ALY0MYQV
repo=$(cd "$(dirname "$0")/.." && pwd)
bin=$repo/bin/multicam-android
remote=/data/local/tmp/multicam
mc=/data/local/tmp/mc
config=$mc/multicam.toml
hold=$mc/hold
wifi=/data/local/tmp/wifi
service=/data/adb/service.d/multicam.sh
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

# The binary is renamed over the old one, so a running server keeps its inode.
adb push "$bin" "$remote.new" >/dev/null || exit 1
adb shell "mv $remote.new $remote && chmod 755 $remote" || exit 1
adb shell "mkdir -p $mc" || exit 1
adb push "$repo/multicam.phone.toml" "$config" >/dev/null || exit 1
# The config holds the bodies' passwords; a push keeps an existing file's mode.
adb shell "chmod 600 $config" || exit 1
adb push "$repo/phone/multicam.sh" "$mc/multicam.sh" >/dev/null || exit 1

if [[ -d $repo/bin/wifi ]]; then
	adb shell "mkdir -p $wifi" || exit 1
	adb push "$repo/bin/wifi/bin" "$repo/bin/wifi/lib" "$wifi/" >/dev/null || exit 1
	adb shell "chmod 755 $wifi/bin/*" || exit 1
else
	echo "bin/wifi is missing: the phone's copy of $wifi is used" >&2
fi

# service.d is root's: Magisk runs only root-owned executables there.
adb shell "su -c 'cp $mc/multicam.sh $service && chown root:root $service &&
	chmod 755 $service'" || exit 1

adb forward "tcp:$port" "tcp:$port" >/dev/null || exit 1

# Starts the supervisor detached, all three streams redirected so adb returns.
# A running one signals the first and exits, which clears its crash count.
start_supervisor() {
	adb shell "su -c 'sh $service </dev/null >/dev/null 2>&1 &'"
}

if [[ $foreground == 0 ]]; then
	start_supervisor || exit 1
	adb shell "su -c 'pkill -x multicam'"
	echo "Console at http://localhost:$port/ (the supervisor restarts the server)"
	exit 0
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
	adb shell "su -c 'rm -f $hold'"
	adb forward --remove "tcp:$port" >/dev/null 2>&1
	echo "The supervisor runs the server again"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

adb shell "su -c 'touch $hold'" || exit 1
start_supervisor || exit 1
if ! stop_remote; then
	echo "could not stop the previous multicam on the phone" >&2
	exit 1
fi
echo "Console at http://localhost:$port/ (Ctrl-C stops multicam on the phone)"

# -t gives the phone side a terminal, so Ctrl-C reaches multicam as SIGINT.
adb shell -t "su -c 'cd $mc && $remote $config'"
rc=$?
echo "multicam exited $rc"
exit $rc
