// Package radv sends IPv6 router advertisements carrying one prefix, so
// nodes on a provisioning network get a stable IPv6 address by SLAAC
// besides their link-local one. stage0 routes nothing, so the router
// lifetime is 0: nodes take the prefix but no default route. Sending needs
// a raw ICMPv6 socket, i.e. CAP_NET_RAW.
package radv

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/net/ipv6"
)

const (
	validLifetime     = 24 * time.Hour
	preferredLifetime = 4 * time.Hour
	interval          = 3 * time.Minute // between unsolicited advertisements
)

// Message is a router advertisement for prefix from mac, the ICMPv6
// checksum left 0 for the kernel to fill.
func Message(prefix netip.Prefix, mac net.HardwareAddr) []byte {
	b := []byte{134, 0, 0, 0, 64, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	b = append(b, 1, 1) // source link-layer address, 8 bytes
	b = append(b, mac[:6]...)
	p := make([]byte, 32) // prefix information, 32 bytes
	p[0], p[1], p[2], p[3] = 3, 4, byte(prefix.Bits()), 0xc0
	binary.BigEndian.PutUint32(p[4:], uint32(validLifetime/time.Second))
	binary.BigEndian.PutUint32(p[8:], uint32(preferredLifetime/time.Second))
	a := prefix.Masked().Addr().As16()
	copy(p[16:], a[:])
	return append(b, p...)
}

// Server advertises one prefix on one interface.
type Server struct {
	prefix netip.Prefix
	log    *slog.Logger
}

// New returns a server for prefix (a /64).
func New(prefix netip.Prefix, log *slog.Logger) *Server {
	return &Server{prefix: prefix, log: log}
}

// Serve advertises on ifname now, every few minutes and in answer to each
// router solicitation, until ctx is done.
func (s *Server) Serve(ctx context.Context, ifname string) error {
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		return err
	}
	if len(ifc.HardwareAddr) != 6 && ifc.Flags&net.FlagLoopback == 0 {
		return fmt.Errorf("radv: %s has no Ethernet address", ifname)
	}
	mac := ifc.HardwareAddr
	if len(mac) != 6 {
		mac = make(net.HardwareAddr, 6) // loopback, in tests
	}
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
		})
		return errors.Join(err, serr)
	}}
	conn, err := lc.ListenPacket(ctx, "ip6:ipv6-icmp", "::")
	if err != nil {
		return fmt.Errorf("radv: raw ICMPv6 socket on %s (needs CAP_NET_RAW): %w", ifname, err)
	}
	defer conn.Close()
	p := ipv6.NewPacketConn(conn)
	// Neighbour discovery requires hop limit 255 (RFC 4861).
	if err := errors.Join(p.SetMulticastHopLimit(255), p.SetHopLimit(255), p.SetMulticastInterface(ifc)); err != nil {
		return fmt.Errorf("radv: %w", err)
	}
	allRouters := &net.IPAddr{IP: net.ParseIP("ff02::2")}
	if err := p.JoinGroup(ifc, allRouters); err != nil && ifc.Flags&net.FlagLoopback == 0 {
		return fmt.Errorf("radv: join ff02::2: %w", err)
	}
	var f ipv6.ICMPFilter
	f.SetAll(true)
	f.Accept(ipv6.ICMPTypeRouterSolicitation)
	p.SetICMPFilter(&f)

	msg := Message(s.prefix, mac)
	allNodes := &net.IPAddr{IP: net.ParseIP("ff02::1"), Zone: ifname}
	send := func(why string) {
		if _, err := p.WriteTo(msg, nil, allNodes); err != nil {
			s.log.Warn("radv: send", "err", err)
			return
		}
		s.log.Debug("radv: advertised", "prefix", s.prefix, "why", why)
	}
	s.log.Info("radv: advertising", "interface", ifname, "prefix", s.prefix)
	send("start")

	solicited := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, _, err := p.ReadFrom(buf); err != nil {
				return // closed
			}
			select {
			case solicited <- struct{}{}:
			default:
			}
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			send("interval")
		case <-solicited:
			send("solicitation")
		}
	}
}
