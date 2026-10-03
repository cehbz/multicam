#!/system/bin/sh
# Runs as root on the phone, detached, for as long as the rig runs (rig.sh
# starts and stops it): keeps each camera in links.conf joined as a Wi-Fi
# Direct client on its own interface. Once a second it reads each link's
# state. A link without a wpa_supplicant gets one; a link neither joined nor
# joining gets a join; a link that has just joined gets its address leased
# and the camera's address routed through it for sockets bound to it.
# Usage: links.sh [links.conf]
BIN=/data/data/com.termux/files/usr/bin
PATH=$BIN:/data/adb/magisk:$PATH
D=$(cd "$(dirname "$0")" && pwd)
CONF=${1:-$D/links.conf}
CAMERA=192.168.122.1
SETSID=$(command -v setsid)

cameras() { grep -v -E '^[[:space:]]*(#|$)' "$CONF"; }
cli() {
	iface=$1
	shift
	wpa_cli -p "$D/ctrl" -i "$iface" "$@" 2>/dev/null
}
state() { cli "$1" status | sed -n 's/^wpa_state=//p'; }
addr() { ip -4 addr show dev "$1" 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p'; }

# The Wi-Fi firmware restarts when a fourth interface comes up: wlan0 holds
# the home network, which leaves two for cameras.
if [ "$(cameras | wc -l)" -gt 2 ]; then
	echo "links.conf lists more than two cameras" >&2
	exit 1
fi

mkdir -p "$D/ctrl"
umask 077

# Starts the wpa_supplicant of IF for SSID, adding the interface if it is
# missing, and waits up to 5 s for it to answer.
supplicant() {
	IF=$1 SSID=$2 PSK=$3
	if [ ! -d "/sys/class/net/$IF" ] && ! iw phy phy0 interface add "$IF" type managed; then
		echo "$IF: cannot add the interface"
		return 1
	fi
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
	# Detached, as rig.sh starts its daemons: a stop that ends a running
	# start leaves the links up.
	($SETSID nohup wpa_supplicant -Dnl80211 -i"$IF" -c "$D/$IF.conf" >"$D/$IF.log" 2>&1 </dev/null &) >/dev/null 2>&1 </dev/null
	i=0
	while [ -z "$(state "$IF")" ] && [ $i -lt 5 ]; do
		sleep 1
		i=$((i + 1))
	done
}

# Leases the address of IF and routes the camera through IF in table t.
lease() {
	IF=$1 t=$2
	busybox udhcpc -i "$IF" -n -q -t 5 -T 2 -s "$D/udhcpc.sh" >/dev/null 2>&1
	ip route replace "$CAMERA/32" dev "$IF" table "$t"
	ip rule del pref 9001 oif "$IF" 2>/dev/null
	ip rule add pref 9001 oif "$IF" lookup "$t"
}

# Checks each link once. $IF.joined marks a link leased since it joined.
keep() {
	t=2000
	cameras | while read -r IF SSID PSK; do
		t=$((t + 1))
		s=$(state "$IF")
		if [ -z "$s" ]; then
			supplicant "$IF" "$SSID" "$PSK" || continue
			s=$(state "$IF")
		fi
		if [ "$s" != COMPLETED ] && [ -e "$D/$IF.joined" ]; then
			rm -f "$D/$IF.joined"
			echo "$IF: lost $SSID"
		fi
		case $s in
		COMPLETED)
			if [ ! -e "$D/$IF.joined" ]; then
				lease "$IF" $t
				touch "$D/$IF.joined"
				echo "$IF: joined $SSID as $(addr "$IF")"
			fi
			;;
		DISCONNECTED | INACTIVE | INTERFACE_DISABLED | "")
			cli "$IF" p2p_group_add persistent=0 >/dev/null
			echo "$IF: joining $SSID"
			;;
		esac
	done
}

# A link joined before this keeper started is leased again.
rm -f "$D"/*.joined
while :; do
	keep
	sleep 1
done
