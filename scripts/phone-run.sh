#!/usr/bin/env bash
# Cross-build multicam, push it to the phone over adb and run it there in the
# foreground, with the console port forwarded to this Mac. Ctrl-C stops it.
# Usage: scripts/phone-run.sh
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
bin=$repo/bin/multicam-android
remote=/data/local/tmp/multicam
port=8080

(cd "$repo" && CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o "$bin" ./cmd/multicam) || exit 1
adb get-state >/dev/null || exit 1

# SIGTERM, up to 5 s for a clean exit, then SIGKILL; fails if it survives.
stop_remote() {
	adb shell 'pkill -x multicam
		for i in 1 2 3 4 5; do pidof multicam >/dev/null || exit 0; sleep 1; done
		pkill -9 -x multicam; sleep 1; ! pidof multicam >/dev/null'
}

cleaned=0
cleanup() {
	[[ $cleaned == 1 ]] && return
	cleaned=1
	if stop_remote; then
		echo "multicam is not running on the phone"
	else
		echo "warning: multicam is still running on the phone" >&2
		echo "  run: adb shell pkill -9 -x multicam" >&2
	fi
	adb forward --remove "tcp:$port" >/dev/null 2>&1
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if ! stop_remote; then
	echo "could not stop the previous multicam on the phone" >&2
	exit 1
fi
adb push "$bin" "$remote" || exit 1
adb forward "tcp:$port" "tcp:$port" >/dev/null || exit 1
echo "Console at http://localhost:$port/ (Ctrl-C stops multicam on the phone)"

# -t gives the phone side a terminal, so Ctrl-C reaches multicam as SIGINT.
adb shell -t "$remote"
rc=$?
echo "multicam exited $rc"
exit $rc
