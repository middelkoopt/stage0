package radv

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestMessage(t *testing.T) {
	mac, _ := net.ParseMAC("d8:3a:dd:98:09:77")
	got := Message(netip.MustParsePrefix("fd00:10:3::/64"), mac)
	want := []byte{
		134, 0, 0, 0, // router advertisement, checksum (the kernel fills it)
		64, 0, 0, 0, // hop limit 64, no M/O flags, router lifetime 0: not a default router
		0, 0, 0, 0, 0, 0, 0, 0, // reachable time, retrans timer: unspecified
		1, 1, 0xd8, 0x3a, 0xdd, 0x98, 0x09, 0x77, // source link-layer address
		3, 4, 64, 0xc0, // prefix information: /64, on-link and autonomous (SLAAC)
		0, 1, 0x51, 0x80, // valid lifetime 86400 s
		0, 0, 0x38, 0x40, // preferred lifetime 14400 s
		0, 0, 0, 0, // reserved
		0xfd, 0, 0, 0x10, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("message\n got % x\nwant % x", got, want)
	}
}

func TestServeNeedsRawSocket(t *testing.T) {
	// Without CAP_NET_RAW, Serve fails at once with a clear error; with it
	// (and the interface), it runs until ctx is done.
	ifname := os.Getenv("STAGE0_TEST_RADV_IF")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := New(netip.MustParsePrefix("fd00:10:3::/64"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if ifname == "" {
		ifname = "lo"
	}
	err := s.Serve(ctx, ifname)
	switch {
	case err == nil:
		t.Logf("sent router advertisements on %s", ifname)
	case errors.Is(err, syscall.EPERM):
		t.Logf("no CAP_NET_RAW, as expected for a test binary: %v", err)
	default:
		t.Errorf("Serve: %v", err)
	}
}
