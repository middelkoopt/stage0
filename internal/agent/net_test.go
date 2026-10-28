package agent

import (
	"bytes"
	"net/netip"
	"testing"
	"time"
)

func TestNewAddrMessage(t *testing.T) {
	got := newAddrMessage(3, netip.MustParsePrefix("10.0.9.100/24"), 7)
	want := []byte{
		40, 0, 0, 0, // length 40
		20, 0, // RTM_NEWADDR
		0x05, 0x06, // NLM_F_REQUEST|NLM_F_ACK|NLM_F_EXCL|NLM_F_CREATE
		7, 0, 0, 0, // sequence
		0, 0, 0, 0, // port id: the kernel picks
		2, 24, 0, 0, // AF_INET, /24, flags, scope universe
		3, 0, 0, 0, // interface index
		8, 0, 2, 0, 10, 0, 9, 100, // IFA_LOCAL
		8, 0, 1, 0, 10, 0, 9, 100, // IFA_ADDRESS
	}
	if !bytes.Equal(got, want) {
		t.Errorf("message\n got % x\nwant % x", got, want)
	}
}

func TestGlobalIPv6(t *testing.T) {
	addrs := []netip.Prefix{
		netip.MustParsePrefix("fe80::1/64"),
		netip.MustParsePrefix("10.0.3.100/24"),
		netip.MustParsePrefix("fd00:10:3:0:da3a:ddff:fec5:c1f3/64"),
	}
	if a, ok := firstGlobal6(addrs); !ok || a.String() != "fd00:10:3:0:da3a:ddff:fec5:c1f3" {
		t.Errorf("got %v %v", a, ok)
	}
	if _, ok := firstGlobal6(addrs[:2]); ok {
		t.Error("link-local taken as global")
	}
}

func TestWaitRoute(t *testing.T) {
	if err := waitRoute("http://127.0.0.1/stage0/report", time.Second); err != nil {
		t.Error(err)
	}
	if err := waitRoute("http://[::1]:8080/x", time.Second); err != nil {
		t.Error(err)
	}
}
