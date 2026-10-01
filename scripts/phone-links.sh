#!/usr/bin/env bash
# Join the rig's cameras on the phone: push the link scripts and
# phone/links.conf over adb and run links.sh there as root.
# Usage: scripts/phone-links.sh
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
remote=/data/local/tmp/mc

if [[ ! -f $repo/phone/links.conf ]]; then
	echo "copy phone/links.example.conf to phone/links.conf and fill in the cameras" >&2
	exit 1
fi
adb get-state >/dev/null || exit 1
adb shell "mkdir -p $remote" || exit 1
if ! adb push "$repo/phone/links.sh" "$repo/phone/udhcpc.sh" "$repo/phone/links.conf" "$remote/" >/dev/null 2>&1; then
	echo "could not push the link scripts to $remote" >&2
	exit 1
fi
adb shell "su -c 'chmod 600 $remote/links.conf; chmod 755 $remote/udhcpc.sh; sh $remote/links.sh'"
