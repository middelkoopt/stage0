// Package dhcpd answers DHCPv4 for nodes network-booting on one provisioning
// network. It answers PXE clients (a Raspberry Pi's bootloader is one), and
// afterwards the same machine's kernel or agent (ip=dhcp sends no vendor
// class); a machine that never PXE-booted from it is never given a lease.
// Leases live in memory: a test bench restarts often and has few nodes.
package dhcpd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// Config describes the network served.
type Config struct {
	Server              netip.Addr   // stage0's own address: server id, next server, TFTP
	Subnet              netip.Prefix // the network
	PoolFirst, PoolLast netip.Addr   // addresses to lease
	LeaseTime           time.Duration
	NTP                 []netip.Addr // NTP servers to hand out (option 42), if any

	// Installed nodes (MAC -> address) boot from their disks: their PXE
	// requests get no answer, their systems' plain requests get their
	// address for InstalledLeaseTime (default LeaseTime).
	Installed          map[string]netip.Addr
	InstalledLeaseTime time.Duration
}

// UEFIBootFile is the file UEFI clients (aarch64) are told to netboot:
// the UKI of stage0's agent, at the top of what serve offers.
const UEFIBootFile = "uki.efi"

// AgentClass is the vendor class stage0's agent sends when it asks for an
// address itself.
const AgentClass = "stage0-agent"

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

	mu        sync.Mutex
	leases    map[string]netip.Addr // MAC -> address
	installed map[string]bool
	ipv6Boot  map[string]bool       // MACs netbooting over IPv6 alone (BootIPv6)
	acked     map[string]netip.Addr // MAC -> address last acknowledged
}

// New returns a server for cfg that logs to log.
func New(cfg Config, log *slog.Logger) *Server {
	if cfg.InstalledLeaseTime == 0 {
		cfg.InstalledLeaseTime = cfg.LeaseTime
	}
	s := &Server{
		cfg: cfg, log: log,
		replyTo:   func(net.Addr) net.Addr { return &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort} },
		leases:    map[string]netip.Addr{},
		installed: map[string]bool{},
		ipv6Boot:  map[string]bool{},
		acked:     map[string]netip.Addr{},
	}
	for mac, ip := range cfg.Installed {
		s.leases[mac], s.installed[mac] = ip, true
	}
	return s
}

// Install marks the node with mac installed (see Config.Installed) and
// returns its address; false if it has no lease.
func (s *Server) Install(mac net.HardwareAddr) (netip.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip, ok := s.leases[mac.String()]
	if ok {
		s.installed[mac.String()] = true
		delete(s.acked, mac.String()) // Acked now reports the installed system
	}
	return ip, ok
}

// InstalledNodes returns the installed nodes' MACs.
func (s *Server) InstalledNodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for mac := range s.installed {
		out = append(out, mac)
	}
	return out
}

// Reserve gives mac a lease if it has none and marks it installed: for a
// node that netbooted over IPv6, so its installed system's DHCPv4 request
// is answered.
func (s *Server) Reserve(mac net.HardwareAddr) (netip.Addr, bool) {
	ip, ok := s.lease(mac.String())
	if !ok {
		return ip, false
	}
	return s.Install(mac)
}

// BootIPv6 makes the node with mac netboot over IPv6 alone: its DHCPv4
// boot requests (PXE, UEFI HTTP boot) go unanswered, as UEFI tries IPv4
// first.
func (s *Server) BootIPv6(mac net.HardwareAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipv6Boot[mac.String()] = true
}

// IsInstalled reports whether mac is an installed node.
func (s *Server) IsInstalled(mac net.HardwareAddr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installed[mac.String()]
}

// Acked returns the address last acknowledged to mac.
func (s *Server) Acked(mac net.HardwareAddr) (netip.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip, ok := s.acked[mac.String()]
	return ip, ok
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
	// HTTPClient: UEFI HTTP boot, which stage0 treats as PXE.
	pxe := strings.HasPrefix(req.ClassIdentifier(), "PXEClient") || strings.HasPrefix(req.ClassIdentifier(), "HTTPClient")
	// stage0's agent when it booted without PXE (a VM given -kernel):
	// a node to lease to, but one that needs no boot options.
	agent := req.ClassIdentifier() == AgentClass
	s.mu.Lock()
	installed := s.installed[mac]
	s.mu.Unlock()
	s.mu.Lock()
	v6 := s.ipv6Boot[mac]
	s.mu.Unlock()
	if pxe && installed {
		log.Debug("dhcp: ignored, installed node (it boots from its disk)", "type", req.MessageType())
		return nil
	}
	if pxe && v6 {
		log.Debug("dhcp: ignored, the node netboots over IPv6", "type", req.MessageType())
		return nil
	}
	if !pxe && !agent {
		s.mu.Lock()
		_, booted := s.leases[mac]
		s.mu.Unlock()
		if !booted {
			log.Debug("dhcp: ignored, not a PXE client and never booted here", "type", req.MessageType())
			return nil
		}
	}

	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		ip, ok := s.lease(mac)
		if !ok {
			log.Warn("dhcp: discover ignored, pool is full")
			return nil
		}
		log.Info("dhcp: offer", "ip", ip)
		return s.reply(req, dhcpv4.MessageTypeOffer, ip, pxe)

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
			return s.reply(req, dhcpv4.MessageTypeNak, netip.Addr{}, pxe)
		}
		log.Info("dhcp: ack", "ip", ip, "installed", installed)
		s.mu.Lock()
		s.acked[mac] = ip
		s.mu.Unlock()
		return s.reply(req, dhcpv4.MessageTypeAck, ip, pxe)
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

func (s *Server) leaseTime(mac string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installed[mac] {
		return s.cfg.InstalledLeaseTime
	}
	return s.cfg.LeaseTime
}

// reply builds an answer; pxe adds the boot options a PXE client needs.
func (s *Server) reply(req *dhcpv4.DHCPv4, typ dhcpv4.MessageType, ip netip.Addr, pxe bool) *dhcpv4.DHCPv4 {
	server := net.IP(s.cfg.Server.AsSlice())
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(typ),
		dhcpv4.WithBroadcast(true),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(server)),
	}
	if typ != dhcpv4.MessageTypeNak {
		mods = append(mods,
			dhcpv4.WithYourIP(net.IP(ip.AsSlice())),
			dhcpv4.WithNetmask(net.CIDRMask(s.cfg.Subnet.Bits(), 32)),
			dhcpv4.WithLeaseTime(uint32(s.leaseTime(req.ClientHWAddr.String())/time.Second)),
		)
		if len(s.cfg.NTP) > 0 {
			ntp := make([]net.IP, len(s.cfg.NTP))
			for i, a := range s.cfg.NTP {
				ntp[i] = net.IP(a.AsSlice())
			}
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptNTPServers(ntp...)))
		}
	}
	if typ != dhcpv4.MessageTypeNak && pxe {
		mods = append(mods,
			dhcpv4.WithServerIP(server),
			dhcpv4.WithOption(dhcpv4.OptTFTPServerName(s.cfg.Server.String())),
		)
		if slices.Contains(req.ClientArch(), iana.EFI_ARM64_HTTP) {
			// UEFI HTTP boot: the boot file is a URL on serve's HTTP, and
			// the reply must carry the HTTPClient class.
			u := "http://" + s.cfg.Server.String() + "/" + UEFIBootFile
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("HTTPClient")),
				dhcpv4.WithOption(dhcpv4.OptBootFileName(u)),
				func(d *dhcpv4.DHCPv4) { d.BootFileName = u })
		} else if slices.Contains(req.ClientArch(), iana.EFI_ARM64) {
			// UEFI loads the named file, stage0's UKI, over TFTP; named in
			// the header's file field and option 67, as firmware reads
			// either. A plain DHCP offer: with the PXEClient class but no
			// option 43, edk2 expects a boot server (BINL) and drops it.
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptBootFileName(UEFIBootFile)),
				func(d *dhcpv4.DHCPv4) { d.BootFileName = UEFIBootFile })
		} else {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
				dhcpv4.WithGeneric(dhcpv4.OptionVendorSpecificInformation, piBoot))
		}
	}
	r, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil { // only fails generating a transaction id, which a reply copies
		s.log.Error("dhcp: building reply", "err", err)
		return nil
	}
	return r
}
