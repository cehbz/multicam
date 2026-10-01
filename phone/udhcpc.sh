#!/system/bin/sh
# udhcpc event script: put the leased address on the interface.
case "$1" in
bound | renew) ip addr replace "$ip/${mask:-24}" dev "$interface" ;;
esac
