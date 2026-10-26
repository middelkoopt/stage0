// Package dhcpd answers DHCPv4 for nodes network-booting on one provisioning
// network. It answers only PXE clients (a Raspberry Pi's bootloader is one),
// so other machines that share the network are never given a lease. Leases
// live in memory: a test bench restarts often and has few nodes.
package dhcpd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// Config describes the network served.
type Config struct {
	Server              netip.Addr   // stage0's own address: server id, next server, TFTP
	Subnet              netip.Prefix // the network
	PoolFirst, PoolLast netip.Addr   // addresses to lease
	LeaseTime           time.Duration
}

// piBoot is option 43 for a Raspberry Pi: PXE discovery control 3, menu
// prompt "PXE" with timeout 0, and one boot menu item "Raspberry Pi Boot".
// The bootloader looks for that item; these are the bytes dnsmasq's
// pxe-service=0,"Raspberry Pi Boot" sends, as used in this lab.
var piBoot = func() []byte {
	b := []byte{6, 1, 3, 10, 4, 0, 'P', 'X', 'E', 9, 20, 0, 0, 17}
	return append(append(b, "Raspberry Pi Boot"...), 0xff)
}()

// Server hands out leases and builds replies.
type Server struct {
	cfg Config
	log *slog.Logger

	// replyTo picks where a reply goes; replies are broadcast, since the
	// client has no address yet. Tests point it at a loopback socket.
	replyTo func(from net.Addr) net.Addr

	mu     sync.Mutex
	leases map[string]netip.Addr // MAC -> address
}

// New returns a server for cfg that logs to log.
func New(cfg Config, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, log: log,
		replyTo: func(net.Addr) net.Addr { return &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort} },
		leases:  map[string]netip.Addr{},
	}
}

// Listen opens the DHCP server port pinned to one interface, so stage0 hears
// and answers only that network. It needs no root where the host lets users
// bind low ports (net.ipv4.ip_unprivileged_port_start) or the binary has
// CAP_NET_BIND_SERVICE.
func Listen(ctx context.Context, ifname string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}
		})
		return errors.Join(err, serr)
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf(":%d", dhcpv4.ServerPort))
	if err != nil {
		return nil, fmt.Errorf("dhcp on %s: %w", ifname, err)
	}
	return pc, nil
}

// Serve answers requests on conn until ctx is done.
func (s *Server) Serve(ctx context.Context, conn net.PacketConn) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("dhcp: %w", err)
		}
		req, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			s.log.Debug("dhcp: not a DHCP packet", "from", from, "err", err)
			continue
		}
		reply := s.Reply(req)
		if reply == nil {
			continue
		}
		if _, err := conn.WriteTo(reply.ToBytes(), s.replyTo(from)); err != nil {
			s.log.Warn("dhcp: send failed", "mac", req.ClientHWAddr, "err", err)
		}
	}
}

// Reply returns the answer to req, or nil when stage0 should stay silent.
func (s *Server) Reply(req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	if req.OpCode != dhcpv4.OpcodeBootRequest || len(req.ClientHWAddr) != 6 {
		return nil
	}
	mac := req.ClientHWAddr.String()
	log := s.log.With("mac", mac, "class", req.ClassIdentifier())
	if !strings.HasPrefix(req.ClassIdentifier(), "PXEClient") {
		log.Debug("dhcp: ignored, not a PXE client", "type", req.MessageType())
		return nil
	}

	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		ip, ok := s.lease(mac)
		if !ok {
			log.Warn("dhcp: discover ignored, pool is full")
			return nil
		}
		log.Info("dhcp: offer", "ip", ip)
		return s.reply(req, dhcpv4.MessageTypeOffer, ip)

	case dhcpv4.MessageTypeRequest:
		if id := req.ServerIdentifier(); id != nil && !id.Equal(s.cfg.Server.AsSlice()) {
			log.Debug("dhcp: request is for another server", "server", id)
			return nil
		}
		want := req.RequestedIPAddress()
		if want == nil || want.IsUnspecified() {
			want = req.ClientIPAddr
		}
		s.mu.Lock()
		ip, ok := s.leases[mac]
		s.mu.Unlock()
		if !ok || !want.Equal(ip.AsSlice()) {
			log.Info("dhcp: nak", "requested", want, "leased", ip)
			return s.reply(req, dhcpv4.MessageTypeNak, netip.Addr{})
		}
		log.Info("dhcp: ack", "ip", ip)
		return s.reply(req, dhcpv4.MessageTypeAck, ip)
	}
	log.Debug("dhcp: ignored", "type", req.MessageType())
	return nil
}

// lease returns mac's address, giving it the first free one if it has none.
func (s *Server) lease(mac string) (netip.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip, ok := s.leases[mac]; ok {
		return ip, true
	}
	used := map[netip.Addr]bool{}
	for _, ip := range s.leases {
		used[ip] = true
	}
	for ip := s.cfg.PoolFirst; !s.cfg.PoolLast.Less(ip); ip = ip.Next() {
		if !used[ip] {
			s.leases[mac] = ip
			return ip, true
		}
	}
	return netip.Addr{}, false
}

func (s *Server) reply(req *dhcpv4.DHCPv4, typ dhcpv4.MessageType, ip netip.Addr) *dhcpv4.DHCPv4 {
	server := net.IP(s.cfg.Server.AsSlice())
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(typ),
		dhcpv4.WithBroadcast(true),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(server)),
	}
	if typ != dhcpv4.MessageTypeNak {
		mods = append(mods,
			dhcpv4.WithYourIP(net.IP(ip.AsSlice())),
			dhcpv4.WithServerIP(server),
			dhcpv4.WithNetmask(net.CIDRMask(s.cfg.Subnet.Bits(), 32)),
			dhcpv4.WithLeaseTime(uint32(s.cfg.LeaseTime/time.Second)),
			dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
			dhcpv4.WithOption(dhcpv4.OptTFTPServerName(s.cfg.Server.String())),
			dhcpv4.WithGeneric(dhcpv4.OptionVendorSpecificInformation, piBoot),
		)
	}
	r, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil { // only fails generating a transaction id, which a reply copies
		s.log.Error("dhcp: building reply", "err", err)
		return nil
	}
	return r
}
