// Package neigh finds a machine's IPv6 addresses on one interface from its
// MAC address, so a person can reach an installed node by its link-local
// address. An installed system usually picks a "stable privacy" address
// that can't be worked out from its MAC, so it is found on the wire: one
// echo to all nodes (ff02::1), one to each responder (so the kernel learns
// its MAC), then the kernel's neighbour table. No root is needed where
// net.ipv4.ping_group_range allows unprivileged ICMP sockets.
package neigh

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
)

// Neighbor is an entry of the kernel's IPv6 neighbour table.
type Neighbor struct {
	Index int // interface index
	Addr  netip.Addr
	MAC   net.HardwareAddr
}

// Find returns the IPv6 addresses (link-local first) that the machine with
// one of macs answers on, on interface ifname, giving up when ctx is done.
func Find(ctx context.Context, ifname string, macs []net.HardwareAddr) ([]netip.Addr, error) {
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	c, err := icmp.ListenPacket("udp6", "::%"+ifname)
	if err != nil {
		return nil, fmt.Errorf("neigh: ICMPv6 socket (is net.ipv4.ping_group_range open?): %w", err)
	}
	defer c.Close()
	for {
		if err := probe(c, ifname); err != nil {
			return nil, err
		}
		table, err := Table()
		if err != nil {
			return nil, err
		}
		if found := match(table, ifc.Index, macs); len(found) > 0 {
			return found, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("neigh: nothing with MAC %v answered on %s: %w", macs, ifname, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// probe pings all nodes on the interface, then each one that answers.
func probe(c *icmp.PacketConn, ifname string) error {
	echo := func(dst netip.Addr) error {
		m := icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: 1, Seq: 1, Data: []byte("stage0")}}
		b, err := m.Marshal(nil)
		if err != nil {
			return err
		}
		_, err = c.WriteTo(b, &net.UDPAddr{IP: dst.AsSlice(), Zone: ifname})
		return err
	}
	if err := echo(netip.MustParseAddr("ff02::1")); err != nil {
		return fmt.Errorf("neigh: ping ff02::1%%%s: %w", ifname, err)
	}
	seen := map[netip.Addr]bool{}
	buf := make([]byte, 1500)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.SetReadDeadline(deadline)
		_, from, err := c.ReadFrom(buf)
		if err != nil {
			break // the deadline
		}
		if u, ok := from.(*net.UDPAddr); ok {
			if a, ok := netip.AddrFromSlice(u.IP); ok && !seen[a] {
				seen[a] = true
				echo(a) // unicast, so the kernel resolves its MAC
			}
		}
	}
	time.Sleep(200 * time.Millisecond)
	return nil
}

// match returns the addresses of entries on interface index with one of
// macs, link-local ones first.
func match(table []Neighbor, index int, macs []net.HardwareAddr) []netip.Addr {
	var out []netip.Addr
	for _, n := range table {
		if n.Index == index && slices.ContainsFunc(macs, func(m net.HardwareAddr) bool { return m.String() == n.MAC.String() }) {
			out = append(out, n.Addr)
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int {
		if a.IsLinkLocalUnicast() != b.IsLinkLocalUnicast() {
			if a.IsLinkLocalUnicast() {
				return -1
			}
			return 1
		}
		return a.Compare(b)
	})
	return slices.Compact(out)
}

// Table reads the kernel's IPv6 neighbour table over netlink.
func Table() ([]Neighbor, error) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_INET6)
	if err != nil {
		return nil, fmt.Errorf("neigh: netlink: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return nil, fmt.Errorf("neigh: netlink: %w", err)
	}
	var out []Neighbor
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWNEIGH {
			continue
		}
		if n, ok := parse(m.Data); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Neighbour states (linux/neighbour.h) that hold no usable MAC.
const (
	nudIncomplete = 0x01
	nudFailed     = 0x20
	nudNoARP      = 0x40
)

// parse reads one ndmsg and its attributes.
func parse(b []byte) (Neighbor, bool) {
	if len(b) < 12 {
		return Neighbor{}, false
	}
	n := Neighbor{Index: int(int32(binary.NativeEndian.Uint32(b[4:])))}
	if binary.NativeEndian.Uint16(b[8:])&(nudIncomplete|nudFailed|nudNoARP) != 0 {
		return Neighbor{}, false
	}
	for a := b[12:]; len(a) >= 4; {
		l := int(binary.NativeEndian.Uint16(a[0:]))
		if l < 4 || l > len(a) {
			return Neighbor{}, false
		}
		v := a[4:l]
		switch binary.NativeEndian.Uint16(a[2:]) {
		case 1: // NDA_DST
			n.Addr, _ = netip.AddrFromSlice(v)
		case 2: // NDA_LLADDR
			n.MAC = net.HardwareAddr(slices.Clone(v))
		}
		a = a[min((l+3)&^3, len(a)):]
	}
	return n, n.Addr.IsValid() && len(n.MAC) == 6
}
