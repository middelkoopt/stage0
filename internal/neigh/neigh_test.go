package neigh

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// ndmsg builds an RTM_NEWNEIGH payload as the kernel sends it.
func ndmsg(ifindex int32, state uint16, dst netip.Addr, mac string) []byte {
	b := make([]byte, 12)
	b[0] = 10 // AF_INET6
	binary.NativeEndian.PutUint32(b[4:], uint32(ifindex))
	binary.NativeEndian.PutUint16(b[8:], state)
	attr := func(typ uint16, v []byte) {
		a := make([]byte, 4, 4+len(v)+3)
		binary.NativeEndian.PutUint16(a[0:], uint16(4+len(v)))
		binary.NativeEndian.PutUint16(a[2:], typ)
		a = append(a, v...)
		for len(a)%4 != 0 {
			a = append(a, 0)
		}
		b = append(b, a...)
	}
	attr(1, dst.AsSlice())
	if mac != "" {
		hw, _ := net.ParseMAC(mac)
		attr(2, hw)
	}
	return b
}

func TestParse(t *testing.T) {
	a := netip.MustParseAddr("fe80::5662:976b:2c19:7e38")
	n, ok := parse(ndmsg(7, 0x02, a, "d8:3a:dd:c5:c1:f3")) // REACHABLE
	if !ok || n.Index != 7 || n.Addr != a || n.MAC.String() != "d8:3a:dd:c5:c1:f3" {
		t.Errorf("parse = %+v, %v", n, ok)
	}
	if _, ok := parse(ndmsg(7, 0x20, a, "d8:3a:dd:c5:c1:f3")); ok { // FAILED
		t.Error("failed entry accepted")
	}
	if _, ok := parse(ndmsg(7, 0x01, a, "")); ok { // INCOMPLETE, no MAC
		t.Error("incomplete entry accepted")
	}
	if _, ok := parse([]byte{1, 2, 3}); ok {
		t.Error("short message accepted")
	}
}

func TestMatch(t *testing.T) {
	hw, _ := net.ParseMAC("d8:3a:dd:c5:c1:f3")
	other, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	table := []Neighbor{
		{Index: 7, Addr: netip.MustParseAddr("2001:db8::1"), MAC: hw},
		{Index: 7, Addr: netip.MustParseAddr("fe80::1"), MAC: hw},
		{Index: 7, Addr: netip.MustParseAddr("fe80::2"), MAC: other},
		{Index: 3, Addr: netip.MustParseAddr("fe80::3"), MAC: hw},
	}
	answered := map[netip.Addr]bool{}
	for _, n := range table {
		answered[n.Addr] = true
	}
	got := match(table, 7, []net.HardwareAddr{hw}, answered)
	if len(got) != 2 || got[0].String() != "fe80::1" || got[1].String() != "2001:db8::1" {
		t.Errorf("match = %v, want link-local first, only interface 7", got)
	}

	// An earlier install's address lingers in the table (STALE) but no
	// longer answers; only this round's answers count.
	stale := append(table, Neighbor{Index: 7, Addr: netip.MustParseAddr("fe80::0:1"), MAC: hw})
	got = match(stale, 7, []net.HardwareAddr{hw}, map[netip.Addr]bool{netip.MustParseAddr("fe80::1"): true})
	if len(got) != 1 || got[0].String() != "fe80::1" {
		t.Errorf("match = %v, want only the address that answered", got)
	}
}

// TestFindLive looks a real machine up when STAGE0_TEST_NEIGH is
// "interface,mac" (e.g. "eth0.3,d8:3a:dd:c5:c1:f3").
func TestFindLive(t *testing.T) {
	spec := os.Getenv("STAGE0_TEST_NEIGH")
	if spec == "" {
		t.Skip(`set STAGE0_TEST_NEIGH to "interface,mac"`)
	}
	ifname, mac, _ := strings.Cut(spec, ",")
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	addrs, err := Find(ctx, ifname, []net.HardwareAddr{hw})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(addrs)
}
