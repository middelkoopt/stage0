package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/pin/tftp/v3"

	"github.com/middelkoopt/stage0/internal/config"
)

func TestServe(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("arm_64bit=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Loopback sockets stand in for the pinned ones.
	dconn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tconn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	app := &App{OpenNetwork: func(_ context.Context, n config.Network) (Sockets, error) {
		if n.Interface != "eth0.3" || n.Server != netip.MustParseAddr("10.0.3.1") {
			t.Errorf("opened %+v", n)
		}
		close(opened)
		return Sockets{DHCP: dconn, TFTP: tconn, HTTP: hln}, nil
	}}

	type result struct {
		out string
		err error
	}
	done := make(chan result)
	go func() {
		out, err := runApp(t, app, "serve", "pinet", "--root", root, "--timeout", "2s")
		done <- result{out, err}
	}()
	<-opened

	c, err := tftp.NewClient(tconn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := c.Receive("/config.txt", "octet")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := wt.WriteTo(&got); err != nil || got.String() != "arm_64bit=1\n" {
		t.Errorf("config.txt = %q, %v", got.String(), err)
	}

	resp, err := http.Get("http://" + hln.Addr().String() + "/config.txt")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "arm_64bit=1\n" {
		t.Errorf("http config.txt = %q", b)
	}
	resp.Body.Close()

	mac, _ := net.ParseMAC("d8:3a:dd:c5:c1:f3")
	d, _ := dhcpv4.NewDiscovery(mac, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00000:UNDI:002001")))
	cli, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.WriteTo(d.ToBytes(), dconn.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("serve: %v\n%s", r.err, r.out)
		}
		for _, want := range []string{"msg=serving", "tftp: sent", "file=/config.txt", "dhcp: offer", "ip=10.0.3.100", "http: sent", "msg=stopped"} {
			if !strings.Contains(r.out, want) {
				t.Errorf("log lacks %q:\n%s", want, r.out)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop at --timeout")
	}
}

func TestServeErrors(t *testing.T) {
	app := func() *App {
		return &App{OpenNetwork: func(context.Context, config.Network) (Sockets, error) {
			t.Error("opened the network")
			return Sockets{}, nil
		}}
	}
	if _, err := runApp(t, app(), "serve", "nope", "--root", t.TempDir()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown network: err = %v", err)
	}
	if _, err := runApp(t, app(), "serve", "pinet", "--root", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing root: want error")
	}
}

func TestCheckInterface(t *testing.T) {
	lo := ""
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			lo = ifc.Name
		}
	}
	if lo == "" {
		t.Skip("no loopback interface")
	}
	if err := checkInterface(lo, netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Errorf("loopback with 127.0.0.1: %v", err)
	}
	if err := checkInterface(lo, netip.MustParseAddr("10.255.255.1")); err == nil || !strings.Contains(err.Error(), "does not carry") {
		t.Errorf("address not on the interface: err = %v", err)
	}
	if err := checkInterface("nope0", netip.MustParseAddr("127.0.0.1")); err == nil || !strings.Contains(err.Error(), "no interface") {
		t.Errorf("missing interface: err = %v", err)
	}
}
