#!/usr/bin/env bash
# Join one camera's AP, run sonyprobe against it, then restore the home network.
# Usage: scripts/probe-camera.sh <body> <ssid> [sonyprobe flags]
set -uo pipefail

if [[ $# -lt 2 ]]; then
	echo "usage: $0 <body> <ssid> [sonyprobe flags, e.g. -record -zoom]" >&2
	exit 2
fi
body=$1
ssid=$2
shift 2
iface=en0
repo=$(cd "$(dirname "$0")/.." && pwd)
probe=$repo/bin/sonyprobe

# Build while still online.
(cd "$repo" && go build -o "$probe" ./cmd/sonyprobe) || exit 1

read -r -s -p "Password for $ssid: " pw
echo

restored=0
restore() {
	[[ $restored == 1 ]] && return
	restored=1
	echo "Restoring Wi-Fi on $iface..."
	networksetup -setairportpower "$iface" off
	sleep 2
	networksetup -setairportpower "$iface" on
	out=$(networksetup -removepreferredwirelessnetwork "$iface" "$ssid" 2>&1)
	status=$?
	if [[ $status -ne 0 || $out == *rror* || $out == *ould\ not* ]]; then
		echo "warning: could not remove $ssid from preferred networks (exit $status): $out" >&2
		echo "  run: sudo networksetup -removepreferredwirelessnetwork $iface '$ssid'" >&2
	else
		echo "$out"
	fi
}
trap restore EXIT
trap 'exit 130' INT TERM

echo "Joining $ssid on $iface..."
out=$(networksetup -setairportnetwork "$iface" "$ssid" "$pw" 2>&1)
status=$?
unset pw
if [[ $status -ne 0 || $out == *ailed* || $out == *rror* || $out == *ould\ not* ]]; then
	# networksetup can exit 0 on a failed join; the message is the signal.
	echo "join failed (exit $status): $out" >&2
	exit 1
fi

netlog=$repo/captures/$body/net-$(date +%Y%m%d-%H%M%S).txt
mkdir -p "$(dirname "$netlog")"
netstate() {
	{
		echo "== $1 $(date +%T)"
		echo "addr: $(ipconfig getifaddr "$iface")  router: $(ipconfig getoption "$iface" router)"
		route -n get 192.168.122.1 | grep -E 'gateway|interface'
		echo "multicast:"; route -n get 239.255.255.250 | grep -E 'gateway|interface'
		arp -an -i "$iface"
		for port in 8080 10000; do nc -z -G 2 -w 2 192.168.122.1 "$port" 2>&1; echo "port $port: exit $?"; done
	} >>"$netlog" 2>&1
}

sleep 5
netstate "after join"
"$probe" -body "$body" -out "$repo/captures" -wait "$@"
rc=$?
netstate "after probe"
echo "sonyprobe exited $rc; network state in $netlog"
exit $rc
