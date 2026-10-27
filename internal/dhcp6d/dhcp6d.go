// Package dhcp6d answers DHCPv6 for nodes netbooting over IPv6 on one
// provisioning network. A Pi's bootloader with USE_IPV6=1 solicits an
// address (IA_NA) and the Bootfile-URL, which names the TFTP server. Only
// clients asking for a Bootfile-URL are answered, so other machines on the
// network get nothing. Leases live in memory, by client DUID.
package dhcp6d

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
	"golang.org/x/net/ipv6"

	"github.com/middelkoopt/stage0/internal/dhcpd"
)

// Config describes the network served.
type Config struct {
	Server netip.Addr       // stage0's address in Prefix: the TFTP server
	Prefix netip.Prefix     // a /64; leases come from <prefix>::100 on
	MAC    net.HardwareAddr // stage0's, for its server DUID
}

const (
	preferred = 4 * time.Hour
	valid     = 24 * time.Hour
)

// Server hands out leases and builds replies.
type Server struct {
	cfg  Config
	duid dhcpv6.DUID
	log  *slog.Logger

	// OnLease, if set, is called with each address handed out and the
	// client's MAC (from its DUID), e.g. to add a neighbour entry.
	OnLease func(ip netip.Addr, mac net.HardwareAddr)

	// IsInstalled, if set, says which nodes are installed: their netboot
	// requests get no answer, so they boot from their disks.
	IsInstalled func(mac net.HardwareAddr) bool

	mu     sync.Mutex
	leases map[string]netip.Addr // client DUID -> address
	next   netip.Addr
}

// New returns a server for cfg that logs to log.
func New(cfg Config, log *slog.Logger) *Server {
	first := cfg.Prefix.Masked().Addr().As16()
	first[14], first[15] = 1, 0 // ::100
	return &Server{
		cfg: cfg, log: log,
		duid:   &dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: cfg.MAC},
		leases: map[string]netip.Addr{},
		next:   netip.AddrFrom16(first),
	}
}

// Listen opens the DHCPv6 server port pinned to ifname and joins the
// all-DHCP-servers group there.
func Listen(ctx context.Context, ifname string) (net.PacketConn, error) {
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
		})
		return errors.Join(err, serr)
	}}
	pc, err := lc.ListenPacket(ctx, "udp6", "[::]:547")
	if err != nil {
		return nil, fmt.Errorf("dhcpv6 on %s: %w", ifname, err)
	}
	if err := ipv6.NewPacketConn(pc).JoinGroup(ifc, &net.UDPAddr{IP: net.ParseIP("ff02::1:2")}); err != nil {
		pc.Close()
		return nil, fmt.Errorf("dhcpv6 on %s: join ff02::1:2: %w", ifname, err)
	}
	return pc, nil
}

// Serve answers on conn until ctx is done; replies go back to the
// client's own (link-local) address.
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
			return fmt.Errorf("dhcpv6: %w", err)
		}
		m, err := dhcpv6.MessageFromBytes(buf[:n])
		if err != nil {
			continue // relayed or malformed
		}
		r := s.Reply(m)
		if r == nil {
			continue
		}
		if _, err := conn.WriteTo(r.ToBytes(), from); err != nil {
			s.log.Warn("dhcpv6: send failed", "to", from, "err", err)
		}
	}
}

// Reply returns the answer to m, or nil when stage0 should stay silent.
func (s *Server) Reply(m *dhcpv6.Message) *dhcpv6.Message {
	cid := m.Options.ClientID()
	if cid == nil || !m.IsOptionRequested(dhcpv6.OptionBootfileURL) {
		return nil
	}
	if sid := m.Options.ServerID(); sid != nil && !sid.Equal(s.duid) {
		return nil // for another server
	}
	log := s.log.With("client", cid.String(), "type", m.Type().String())
	if mac := duidMAC(cid); mac != nil && s.IsInstalled != nil && s.IsInstalled(mac) {
		log.Debug("dhcpv6: ignored, installed node (it boots from its disk)")
		return nil
	}
	var iaid [4]byte
	if ia := m.Options.OneIANA(); ia != nil {
		iaid = ia.IaId
	}
	ip := s.lease(cid.String())
	if s.OnLease != nil {
		if mac := duidMAC(cid); mac != nil {
			s.OnLease(ip, mac)
		}
	}
	mods := []dhcpv6.Modifier{dhcpv6.WithServerID(s.duid)}
	if slices.Contains(m.Options.ArchTypes(), iana.EFI_ARM64_HTTP) {
		// UEFI HTTP boot: the UKI's URL, and the HTTPClient vendor class
		// edk2 looks for (enterprise 343, the one it sends).
		mods = append(mods,
			dhcpv6.WithOption(dhcpv6.OptBootFileURL("http://["+s.cfg.Server.String()+"]/"+dhcpd.UEFIBootFile)),
			dhcpv6.WithOption(&dhcpv6.OptVendorClass{EnterpriseNumber: 343, Data: [][]byte{[]byte("HTTPClient")}}))
	} else {
		mods = append(mods, dhcpv6.WithOption(dhcpv6.OptBootFileURL("tftp://["+s.cfg.Server.String()+"]/")))
	}
	if m.Options.OneIANA() != nil {
		mods = append(mods, dhcpv6.WithOption(&dhcpv6.OptIANA{IaId: iaid, T1: preferred / 2, T2: preferred * 4 / 5,
			Options: dhcpv6.IdentityOptions{Options: dhcpv6.Options{&dhcpv6.OptIAAddress{
				IPv6Addr: ip.AsSlice(), PreferredLifetime: preferred, ValidLifetime: valid}}}}))
	}
	var r *dhcpv6.Message
	var err error
	switch m.Type() {
	case dhcpv6.MessageTypeSolicit:
		r, err = dhcpv6.NewAdvertiseFromSolicit(m, mods...)
		log.Info("dhcpv6: advertise", "ip", ip)
	case dhcpv6.MessageTypeRequest, dhcpv6.MessageTypeRenew, dhcpv6.MessageTypeRebind,
		dhcpv6.MessageTypeConfirm, dhcpv6.MessageTypeInformationRequest:
		r, err = dhcpv6.NewReplyFromMessage(m, mods...)
		log.Info("dhcpv6: reply", "ip", ip)
	default:
		return nil
	}
	if err != nil {
		log.Warn("dhcpv6: building the answer", "err", err)
		return nil
	}
	return r
}

// lease returns the client's address, giving it the next one if it has none.
func (s *Server) lease(client string) netip.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip, ok := s.leases[client]; ok {
		return ip
	}
	ip := s.next
	s.next = s.next.Next()
	s.leases[client] = ip
	return ip
}

// duidMAC is the MAC in a DUID-LL or DUID-LLT, or nil.
func duidMAC(d dhcpv6.DUID) net.HardwareAddr {
	switch d := d.(type) {
	case *dhcpv6.DUIDLL:
		return d.LinkLayerAddr
	case *dhcpv6.DUIDLLT:
		return d.LinkLayerAddr
	}
	return nil
}
