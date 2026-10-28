package gs316ep

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/middelkoopt/stage0/netswitch"
)

// The fixtures in testdata were captured from a real GS316EP (firmware as of
// 2026-10-25) with the serial number, MAC address and login nonce redacted.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseStatus(t *testing.T) {
	st, err := parseStatus(fixture(t, "dashboard.html"), fixture(t, "poePortConf.html"), fixture(t, "vlan.html"))
	if err != nil {
		t.Fatal(err)
	}

	wantVLANs := []netswitch.VLAN{
		{ID: 1, Name: "Default"}, {ID: 2, Name: "untagged"}, {ID: 3, Name: "pinet"},
		{ID: 4, Name: "cable"}, {ID: 5, Name: "piwulf"}, {ID: 23, Name: "coffee"},
	}
	if !reflect.DeepEqual(st.VLANs, wantVLANs) {
		t.Errorf("VLANs = %+v, want %+v", st.VLANs, wantVLANs)
	}

	if len(st.Ports) != 16 {
		t.Fatalf("got %d ports, want 16", len(st.Ports))
	}
	on, off := true, false
	tests := []netswitch.Port{
		{ID: 1, Name: "k52", PoE: &off, PVID: 5, Tagged: []int{3, 23}},
		{ID: 2, Name: "k53", Linked: true, Speed: "1000M Full", PoE: &on, PVID: 3, Tagged: []int{5, 23}},
		{ID: 3, Name: "", PoE: &off, PVID: 5, Tagged: []int{3, 23}},
		{ID: 9, Name: "", PoE: &on, PVID: 3, Tagged: []int{2, 23}},
		{ID: 12, Name: "pilab", Linked: true, Speed: "1000M Full", PoE: &off, PVID: 2, Tagged: []int{3, 23}},
		{ID: 14, Name: "cable", Linked: true, Speed: "1000M Full", PoE: &off, PVID: 4, Tagged: []int{}},
		{ID: 16, Name: "", PoE: nil, PVID: 1, Tagged: []int{23}}, // port 16 has no PoE
	}
	for _, want := range tests {
		got := st.Ports[want.ID-1]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("port %d:\n got %+v\nwant %+v", want.ID, describe(got), describe(want))
		}
	}
}

// describe dereferences PoE so failures print the value, not a pointer.
func describe(p netswitch.Port) any {
	type view struct {
		netswitch.Port
		PoE string
	}
	v := view{Port: p, PoE: "none"}
	if p.PoE != nil {
		v.PoE = map[bool]string{true: "on", false: "off"}[*p.PoE]
	}
	return v
}

func TestParseVLANList(t *testing.T) {
	tests := []struct {
		in         string
		pvid       int
		tagged     []int
		wantErrors bool
	}{
		{in: "3, 5*, 23", pvid: 5, tagged: []int{3, 23}},
		{in: "23*", pvid: 23, tagged: []int{}},
		{in: "1*, 23", pvid: 1, tagged: []int{23}},
		{in: "3, 5", wantErrors: true},   // no PVID
		{in: "3*, 5*", wantErrors: true}, // two PVIDs
		{in: "3, x*", wantErrors: true},  // not a number
		{in: "", wantErrors: true},       // empty
	}
	for _, tt := range tests {
		pvid, tagged, err := parseVLANList(tt.in)
		if tt.wantErrors {
			if err == nil {
				t.Errorf("parseVLANList(%q): want error, got pvid=%d tagged=%v", tt.in, pvid, tagged)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVLANList(%q): %v", tt.in, err)
			continue
		}
		if pvid != tt.pvid || !reflect.DeepEqual(tagged, tt.tagged) {
			t.Errorf("parseVLANList(%q) = %d, %v; want %d, %v", tt.in, pvid, tagged, tt.pvid, tt.tagged)
		}
	}
}

func TestParseStatusRejectsUnknownPage(t *testing.T) {
	login := fixture(t, "login.html")
	if _, err := parseStatus(login, login, login); err == nil {
		t.Error("parseStatus of the login page: want error, got nil")
	}
}
