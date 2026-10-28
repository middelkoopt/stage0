#!/bin/sh
# A stand-in for "another DHCP server" on a network, for testing stage0's
# proxy mode (docs/network-modes.md): dnsmasq in its own network namespace
# on a macvlan of the interface, so it doesn't collide with stage0's port
# 67 on the host. It hands out addresses and no boot options.
#
#   tools/standin-dhcp.sh start eth0.3 10.0.3.254/24 10.0.3.50,10.0.3.99 [fd00:10:3::fe/64]
#   tools/standin-dhcp.sh stop
#
# With an IPv6 address it also sends router advertisements for that /64
# (SLAAC only), standing in for the network's router.
#
# Needs root (one sudo per call).
set -eu
ns=s0dhcp
case "${1:-}" in
start)
	ifname=$2 addr=$3 range=$4 addr6=${5:-}
	ra=""
	if [ -n "$addr6" ]; then
		ra="--enable-ra --dhcp-range=${addr6%/*},ra-only,64"
	fi
	sudo sh -c "
		set -e
		ip netns add $ns
		ip link add ${ns}0 link $ifname type macvlan mode bridge
		ip link set ${ns}0 netns $ns
		ip netns exec $ns ip addr add $addr dev ${ns}0
		[ -z '$addr6' ] || ip netns exec $ns ip -6 addr add $addr6 dev ${ns}0 nodad
		ip netns exec $ns ip link set ${ns}0 up
		ip netns exec $ns dnsmasq --port=0 --interface=${ns}0 --bind-interfaces \
			--dhcp-range=$range,1h $ra --dhcp-leasefile=/run/$ns.leases \
			--log-dhcp --log-facility=/run/$ns.log --pid-file=/run/$ns.pid
	"
	echo "dnsmasq on $ifname (namespace $ns): $range; log /run/$ns.log"
	;;
stop)
	sudo sh -c "
		[ -f /run/$ns.pid ] && kill \$(cat /run/$ns.pid) || true
		ip netns del $ns 2>/dev/null || true
		rm -f /run/$ns.pid /run/$ns.leases
	"
	echo "stopped"
	;;
*)
	echo "usage: $0 start <interface> <address/prefix> <first,last> [<address6/64>] | stop" >&2
	exit 2
	;;
esac
