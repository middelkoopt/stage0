package dhcp6d

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
)

func testServer() *Server {
	mac, _ := net.ParseMAC("d8:3a:dd:98:09:77")
	return New(Config{
		Server: netip.MustParseAddr("fd00:10:3::1"), Prefix: netip.MustParsePrefix("fd00:10:3::/64"), MAC: mac,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// solicit builds a Solicit like the Pi 5 bootloader's: DUID-LL, an IA_NA,
// a request for the Bootfile-URL, its architecture.
func solicit(t *testing.T, mac string, wantBoot bool) *dhcpv6.Message {
	t.Helper()
	hw, _ := net.ParseMAC(mac)
	m := &dhcpv6.Message{MessageType: dhcpv6.MessageTypeSolicit, TransactionID: dhcpv6.TransactionID{1, 2, 3}}
	m.AddOption(dhcpv6.OptClientID(&dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: hw}))
	m.AddOption(&dhcpv6.OptIANA{IaId: [4]byte{0, 0, 0, 0}})
	if wantBoot {
		m.AddOption(dhcpv6.OptRequestedOption(dhcpv6.OptionBootfileURL))
		m.AddOption(dhcpv6.OptClientArchType(iana.Arch(0x29)))
	}
	return m
}

func TestAdvertise(t *testing.T) {
	s := testServer()
	r := s.Reply(solicit(t, "d8:3a:dd:c5:c1:f3", true))
	if r == nil {
		t.Fatal("no answer to a Pi's solicit")
	}
	if r.Type() != dhcpv6.MessageTypeAdvertise {
		t.Errorf("type %v", r.Type())
	}
	ia := r.Options.OneIANA()
	if ia == nil || ia.Options.OneAddress() == nil || ia.Options.OneAddress().IPv6Addr.String() != "fd00:10:3::100" {
		t.Fatalf("IA_NA %v", ia)
	}
	if u := r.Options.BootFileURL(); u != "tftp://[fd00:10:3::1]/" {
		t.Errorf("bootfile url %q", u)
	}
	if r.Options.ServerID() == nil || r.Options.ClientID() == nil {
		t.Error("missing server or client id")
	}
	// The same client keeps its address; another gets the next.
	if a := s.Reply(solicit(t, "d8:3a:dd:c5:c1:f3", true)).Options.OneIANA().Options.OneAddress().IPv6Addr.String(); a != "fd00:10:3::100" {
		t.Errorf("same client got %s", a)
	}
	if a := s.Reply(solicit(t, "e4:5f:01:1d:17:a2", true)).Options.OneIANA().Options.OneAddress().IPv6Addr.String(); a != "fd00:10:3::101" {
		t.Errorf("second client got %s", a)
	}
}

func TestRequestReply(t *testing.T) {
	s := testServer()
	adv := s.Reply(solicit(t, "d8:3a:dd:c5:c1:f3", true))
	req, err := dhcpv6.NewRequestFromAdvertise(adv)
	if err != nil {
		t.Fatal(err)
	}
	req.AddOption(dhcpv6.OptRequestedOption(dhcpv6.OptionBootfileURL))
	r := s.Reply(req)
	if r == nil || r.Type() != dhcpv6.MessageTypeReply || r.Options.OneIANA().Options.OneAddress().IPv6Addr.String() != "fd00:10:3::100" {
		t.Fatalf("reply %v", r)
	}
	// A request for another server is not ours.
	other, _ := dhcpv6.NewRequestFromAdvertise(adv)
	hw, _ := net.ParseMAC("00:11:22:33:44:55")
	other.UpdateOption(dhcpv6.OptServerID(&dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: hw}))
	if r := s.Reply(other); r != nil {
		t.Errorf("answered a request to another server: %v", r)
	}
}

func TestIgnoresNonBootClients(t *testing.T) {
	if r := testServer().Reply(solicit(t, "d8:3a:dd:c5:c1:f3", false)); r != nil {
		t.Errorf("answered a client that wants no boot file: %v", r)
	}
}

func TestOnLease(t *testing.T) {
	s := testServer()
	var got []string
	s.OnLease = func(ip netip.Addr, mac net.HardwareAddr) { got = append(got, ip.String()+" "+mac.String()) }
	s.Reply(solicit(t, "d8:3a:dd:c5:c1:f3", true))
	if len(got) != 1 || got[0] != "fd00:10:3::100 d8:3a:dd:c5:c1:f3" {
		t.Errorf("OnLease calls %v", got)
	}
}

func TestIgnoresInstalled(t *testing.T) {
	s := testServer()
	s.IsInstalled = func(mac net.HardwareAddr) bool { return mac.String() == "d8:3a:dd:c5:c1:f3" }
	if r := s.Reply(solicit(t, "d8:3a:dd:c5:c1:f3", true)); r != nil {
		t.Errorf("answered an installed node's netboot: %v", r)
	}
	if r := s.Reply(solicit(t, "e4:5f:01:1d:17:a2", true)); r == nil {
		t.Error("ignored a node that isn't installed")
	}
}
