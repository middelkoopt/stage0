package dhcpd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// piClass is what a Raspberry Pi's bootloader sends as its vendor class.
const piClass = "PXEClient:Arch:00000:UNDI:002001"

func testServer(t *testing.T, poolLast string) *Server {
	t.Helper()
	return New(Config{
		Server:    netip.MustParseAddr("10.0.3.1"),
		Subnet:    netip.MustParsePrefix("10.0.3.0/24"),
		PoolFirst: netip.MustParseAddr("10.0.3.100"),
		PoolLast:  netip.MustParseAddr(poolLast),
		LeaseTime: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func discover(t *testing.T, hw, class string) *dhcpv4.DHCPv4 {
	t.Helper()
	var mods []dhcpv4.Modifier
	if class != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(class)))
	}
	d, err := dhcpv4.NewDiscovery(mac(hw), mods...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func request(t *testing.T, hw string, ip, server string) *dhcpv4.DHCPv4 {
	t.Helper()
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(piClass)),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP(ip))),
	}
	if server != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(server))))
	}
	d, err := dhcpv4.New(append([]dhcpv4.Modifier{dhcpv4.WithHwAddr(mac(hw))}, mods...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOfferToPi(t *testing.T) {
	s := testServer(t, "10.0.3.199")
	r := s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", piClass))
	if r == nil {
		t.Fatal("no reply to a Pi's discover")
	}
	checks := []struct {
		what      string
		got, want any
	}{
		{"type", r.MessageType(), dhcpv4.MessageTypeOffer},
		{"your IP", r.YourIPAddr.String(), "10.0.3.100"},
		{"next server", r.ServerIPAddr.String(), "10.0.3.1"},
		{"server id", r.ServerIdentifier().String(), "10.0.3.1"},
		{"mask", net.IP(r.SubnetMask()).String(), "255.255.255.0"},
		{"lease", r.IPAddressLeaseTime(0), time.Hour},
		{"class", r.ClassIdentifier(), "PXEClient"},
		{"tftp server", r.TFTPServerName(), "10.0.3.1"},
		{"broadcast", r.IsBroadcast(), true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.what, c.got, c.want)
		}
	}
	// The bytes this lab's ISC dhcpd sent (lab ReadMe), which dnsmasq's
	// pxe-service=0,"Raspberry Pi Boot" produces.
	want43 := []byte{6, 1, 3, 10, 4, 0, 'P', 'X', 'E', 9, 20, 0, 0, 17}
	want43 = append(append(want43, "Raspberry Pi Boot"...), 0xff)
	if got := r.GetOneOption(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, want43) {
		t.Errorf("option 43 = % x\nwant        % x", got, want43)
	}
}

func TestLeasesAreStable(t *testing.T) {
	s := testServer(t, "10.0.3.199")
	a := s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", piClass)).YourIPAddr.String()
	b := s.Reply(discover(t, "e4:5f:01:1d:17:a2", piClass)).YourIPAddr.String()
	again := s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", piClass)).YourIPAddr.String()
	if a != "10.0.3.100" || b != "10.0.3.101" || again != a {
		t.Errorf("leases %s, %s, then %s again; want .100, .101, .100", a, b, again)
	}
}

func TestIgnoresNonPXE(t *testing.T) {
	s := testServer(t, "10.0.3.199")
	if r := s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", "")); r != nil {
		t.Errorf("answered a client with no vendor class: %v", r.Summary())
	}
	if r := s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", "android-dhcp-14")); r != nil {
		t.Errorf("answered a non-PXE client: %v", r.Summary())
	}
}

func TestPoolExhausted(t *testing.T) {
	s := testServer(t, "10.0.3.100") // one address
	s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", piClass))
	if r := s.Reply(discover(t, "e4:5f:01:1d:17:a2", piClass)); r != nil {
		t.Errorf("offered %s from an empty pool", r.YourIPAddr)
	}
}

func TestRequest(t *testing.T) {
	s := testServer(t, "10.0.3.199")
	s.Reply(discover(t, "d8:3a:dd:c5:c1:f3", piClass))

	r := s.Reply(request(t, "d8:3a:dd:c5:c1:f3", "10.0.3.100", "10.0.3.1"))
	if r == nil || r.MessageType() != dhcpv4.MessageTypeAck || r.YourIPAddr.String() != "10.0.3.100" {
		t.Errorf("request for the offered address: got %v, want ACK 10.0.3.100", r)
	}
	if r := s.Reply(request(t, "d8:3a:dd:c5:c1:f3", "10.0.3.150", "10.0.3.1")); r == nil || r.MessageType() != dhcpv4.MessageTypeNak {
		t.Errorf("request for another address: got %v, want NAK", r)
	}
	if r := s.Reply(request(t, "d8:3a:dd:c5:c1:f3", "10.0.3.100", "")); r == nil || r.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("request without server id (reboot): got %v, want ACK", r)
	}
	if r := s.Reply(request(t, "d8:3a:dd:c5:c1:f3", "192.168.1.50", "192.168.1.1")); r != nil {
		t.Errorf("request to another server: answered %v", r.Summary())
	}
	if r := s.Reply(request(t, "aa:bb:cc:dd:ee:ff", "10.0.3.100", "")); r == nil || r.MessageType() != dhcpv4.MessageTypeNak {
		t.Errorf("request with no lease: got %v, want NAK", r)
	}
}

func TestServe(t *testing.T) {
	// Over loopback UDP: replies go to the client's address here instead of
	// the broadcast address.
	s := testServer(t, "10.0.3.199")
	srv, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	s.replyTo = func(net.Addr) net.Addr { return cli.LocalAddr() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, srv) }()

	if _, err := cli.WriteTo([]byte("not dhcp"), srv.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.WriteTo(discover(t, "d8:3a:dd:c5:c1:f3", piClass).ToBytes(), srv.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := cli.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	r, err := dhcpv4.FromBytes(buf[:n])
	if err != nil || r.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("got %v (%v), want an offer", r, err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve after cancel: %v", err)
	}
}
