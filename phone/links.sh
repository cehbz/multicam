#!/system/bin/sh
# Runs as root on the phone. Joins each camera in links.conf as a Wi-Fi Direct
# client on its own interface, leases its address and routes the camera's
# address through that interface for sockets bound to it.
# Usage: links.sh [links.conf]
BIN=/data/data/com.termux/files/usr/bin
BB=/data/adb/magisk/busybox
D=$(cd "$(dirname "$0")" && pwd)
CONF=${1:-$D/links.conf}
CAMERA=192.168.122.1

cameras() { grep -v -E '^[[:space:]]*(#|$)' "$CONF"; }
cli() {
	iface=$1
	shift
	$BIN/wpa_cli -p "$D/ctrl" -i "$iface" "$@" 2>/dev/null
}
state() { cli "$1" status | sed -n 's/^wpa_state=//p'; }

# The Wi-Fi firmware restarts when a fourth interface comes up: wlan0 holds
# the home network, which leaves two for cameras.
if [ "$(cameras | wc -l)" -gt 2 ]; then
	echo "links.conf lists more than two cameras" >&2
	exit 1
fi

mkdir -p "$D/ctrl"
umask 077
t=2000
cameras | while read -r IF SSID PSK; do
	t=$((t + 1))
	if [ ! -d "/sys/class/net/$IF" ] && ! $BIN/iw phy phy0 interface add "$IF" type managed; then
		echo "$IF: cannot add the interface"
		continue
	fi
	if [ -z "$(state "$IF")" ]; then
		cat >"$D/$IF.conf" <<CONF
ctrl_interface=$D/ctrl
update_config=0
p2p_no_group_iface=1
network={
	ssid="$SSID"
	psk="$PSK"
	proto=RSN
	key_mgmt=WPA-PSK
	pairwise=CCMP
	mode=0
	disabled=2
}
CONF
		nohup $BIN/wpa_supplicant -Dnl80211 -i"$IF" -c "$D/$IF.conf" >"$D/$IF.log" 2>&1 </dev/null &
		i=0
		while [ -z "$(state "$IF")" ] && [ $i -lt 5 ]; do sleep 1; i=$((i + 1)); done
	fi
	if [ "$(state "$IF")" != COMPLETED ]; then
		cli "$IF" p2p_group_add persistent=0 >/dev/null
		i=0
		while [ "$(state "$IF")" != COMPLETED ] && [ $i -lt 20 ]; do sleep 1; i=$((i + 1)); done
	fi
	if [ "$(state "$IF")" != COMPLETED ]; then
		echo "$IF: $SSID not joined (is the camera on its connection screen?)"
		continue
	fi
	$BB udhcpc -i "$IF" -n -q -t 5 -T 2 -s "$D/udhcpc.sh" >/dev/null 2>&1
	ip route replace "$CAMERA/32" dev "$IF" table $t
	ip rule del pref 9001 oif "$IF" 2>/dev/null
	ip rule add pref 9001 oif "$IF" lookup $t
	echo "$IF: joined $SSID as $(ip -4 addr show dev "$IF" | sed -n 's/.*inet \([0-9.]*\).*/\1/p')"
done
