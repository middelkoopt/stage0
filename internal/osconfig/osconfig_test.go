package osconfig

import (
	"strings"
	"testing"
)

const boot = "D8:3A:DD:C5:C1:F3"

func byPath(t *testing.T, files []File) map[string]File {
	t.Helper()
	m := map[string]File{}
	for _, f := range files {
		if _, dup := m[f.Path]; dup {
			t.Errorf("%s twice", f.Path)
		}
		m[f.Path] = f
	}
	return m
}

func TestDefaultNetworkd(t *testing.T) {
	files, err := Render(System{Hostname: "k52", Keys: []string{"ssh-ed25519 AAAA a@b"}, Network: Networkd}, boot)
	if err != nil {
		t.Fatal(err)
	}
	m := byPath(t, files)
	if m["/etc/hostname"].Data != "k52\n" {
		t.Errorf("hostname %q", m["/etc/hostname"].Data)
	}
	if k := m["/root/.ssh/authorized_keys"]; k.Data != "ssh-ed25519 AAAA a@b\n" || k.Mode != 0o600 {
		t.Errorf("keys %+v", k)
	}
	want := "[Match]\nMACAddress=d8:3a:dd:c5:c1:f3\nType=ether\n\n[Network]\nDHCP=yes\n"
	if got := m["/etc/systemd/network/10-stage0-eth0.network"].Data; got != want {
		t.Errorf("network\n%s\nwant\n%s", got, want)
	}
}

func TestVLANsNetworkd(t *testing.T) {
	sys := System{Hostname: "k52", Network: Networkd, Interfaces: []Interface{{
		Settings: Settings{DHCP: true},
		VLANs: []VLAN{{ID: 4, Settings: Settings{Addresses: []string{"10.0.4.5/24", "fd00:4::5/64"},
			Gateway: "10.0.4.1", DNS: []string{"10.0.4.1"}}}},
	}}}
	files, err := Render(sys, boot)
	if err != nil {
		t.Fatal(err)
	}
	m := byPath(t, files)
	if !strings.Contains(m["/etc/systemd/network/10-stage0-eth0.network"].Data, "DHCP=yes\nVLAN=vlan4\n") {
		t.Errorf("parent %q", m["/etc/systemd/network/10-stage0-eth0.network"].Data)
	}
	if got := m["/etc/systemd/network/10-stage0-vlan4.netdev"].Data; got != "[NetDev]\nName=vlan4\nKind=vlan\n\n[VLAN]\nId=4\n" {
		t.Errorf("netdev %q", got)
	}
	want := "[Match]\nName=vlan4\n\n[Network]\nDHCP=no\nIPv6AcceptRA=no\nAddress=10.0.4.5/24\nAddress=fd00:4::5/64\nGateway=10.0.4.1\nDNS=10.0.4.1\n"
	if got := m["/etc/systemd/network/10-stage0-vlan4.network"].Data; got != want {
		t.Errorf("vlan network\n%s\nwant\n%s", got, want)
	}
}

func TestNetworkManager(t *testing.T) {
	sys := System{Hostname: "k52", Network: NetworkManager, Interfaces: []Interface{{
		VLANs: []VLAN{{ID: 23, Settings: Settings{DHCP: true}}},
	}}}
	files, err := Render(sys, boot)
	if err != nil {
		t.Fatal(err)
	}
	m := byPath(t, files)
	eth := m["/etc/NetworkManager/system-connections/stage0-eth0.nmconnection"]
	if eth.Mode != 0o600 || !strings.Contains(eth.Data, "type=ethernet\n") || !strings.Contains(eth.Data, "mac-address=d8:3a:dd:c5:c1:f3\n") ||
		!strings.Contains(eth.Data, "[ipv4]\nmethod=disabled\n") || !strings.Contains(eth.Data, "[ipv6]\nmethod=disabled\n") {
		t.Errorf("ethernet %+v", eth)
	}
	uuid := between(eth.Data, "uuid=", "\n")
	vlan := m["/etc/NetworkManager/system-connections/stage0-vlan23.nmconnection"].Data
	if !strings.Contains(vlan, "[vlan]\nid=23\nparent="+uuid+"\n") || !strings.Contains(vlan, "[ipv4]\nmethod=auto\n") ||
		!strings.Contains(vlan, "[ipv6]\nmethod=auto\n") {
		t.Errorf("vlan\n%s", vlan)
	}
	// The same node gets the same connection every install.
	again, _ := Render(sys, boot)
	if again[1].Data != files[1].Data {
		t.Error("uuid changed between renders")
	}
}

func TestNetworkManagerStatic(t *testing.T) {
	files, err := Render(System{Hostname: "h", Network: NetworkManager, Interfaces: []Interface{{
		MAC: "52:54:00:12:34:56", Settings: Settings{Addresses: []string{"10.0.23.5/24"}, Gateway: "10.0.23.1", DNS: []string{"10.0.23.1", "fd00::53"}},
	}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := files[1].Data
	for _, want := range []string{"[ipv4]\nmethod=manual\naddress1=10.0.23.5/24\ngateway=10.0.23.1\ndns=10.0.23.1;\n", "[ipv6]\nmethod=link-local\ndns=fd00::53;\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		ifs []Interface
		err string
	}{
		{[]Interface{{}, {}}, "only one interface"},
		{[]Interface{{MAC: "nope"}}, "mac"},
		{[]Interface{{MAC: boot}, {MAC: strings.ToLower(boot)}}, "twice"},
		{[]Interface{{Settings: Settings{Addresses: []string{"10.0.0.5"}}}}, "address"},
		{[]Interface{{Settings: Settings{DHCP: true, Gateway: "x"}}}, "gateway"},
		{[]Interface{{Settings: Settings{DNS: []string{"1.1.1.1"}}}}, "without dhcp"},
		{[]Interface{{VLANs: []VLAN{{ID: 0}}}}, "1-4094"},
		{[]Interface{{VLANs: []VLAN{{ID: 4}, {ID: 4}}}}, "twice"},
	} {
		if err := Check(tt.ifs); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%+v: err %v, want %q", tt.ifs, err, tt.err)
		}
	}
	if _, err := Render(System{Hostname: "h", Network: "netplan"}, boot); err == nil {
		t.Error("unknown network tool accepted")
	}
	if _, err := Render(System{Hostname: "h", Network: Networkd}, ""); err == nil {
		t.Error("no boot MAC accepted")
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	v, _, _ := strings.Cut(rest, b)
	return v
}
