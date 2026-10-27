package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSite(t *testing.T) {
	p := write(t, "site.yaml", `
switches:
  - name: lab
    address: switch.example
`, 0o644)
	s, err := LoadSite(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Switches) != 1 {
		t.Fatalf("switches = %+v", s.Switches)
	}
	sw := s.Switches[0]
	if sw.Name != "lab" || sw.Address != "switch.example" || sw.Driver != "gs316ep" {
		t.Errorf("switch = %+v (driver should default to gs316ep)", sw)
	}
}

func TestLoadSiteErrors(t *testing.T) {
	tests := map[string]string{
		"unknown field":     "switches:\n  - name: a\n    adress: x\n",
		"missing name":      "switches:\n  - address: x\n",
		"missing address":   "switches:\n  - name: a\n",
		"duplicate name":    "switches:\n  - {name: a, address: x}\n  - {name: a, address: y}\n",
		"unknown driver":    "switches:\n  - {name: a, address: x, driver: cisco}\n",
		"unknown top-level": "nodes: []\nswitchs: []\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSite(write(t, "site.yaml", body, 0o644)); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestLoadSiteMissingFile(t *testing.T) {
	_, err := LoadSite(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("err = %v, want it to name the file", err)
	}
}

func TestSiteSwitch(t *testing.T) {
	s := &Site{Switches: []Switch{{Name: "a"}, {Name: "b"}}}
	if sw, err := s.Switch("b"); err != nil || sw.Name != "b" {
		t.Errorf("Switch(b) = %+v, %v", sw, err)
	}
	if sw, err := s.Switch(""); err == nil {
		t.Errorf("Switch(\"\") with two switches = %+v, want ambiguity error", sw)
	}
	if _, err := s.Switch("c"); err == nil {
		t.Error("Switch(c): want error")
	}
	one := &Site{Switches: []Switch{{Name: "only"}}}
	if sw, err := one.Switch(""); err != nil || sw.Name != "only" {
		t.Errorf("Switch(\"\") with one switch = %+v, %v", sw, err)
	}
}

func TestLoadSecrets(t *testing.T) {
	p := write(t, ".secrets", "switches:\n  lab:\n    password: hunter2\n", 0o600)
	s, err := LoadSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Switches["lab"].Password != "hunter2" {
		t.Errorf("secrets = %+v", s)
	}
}

func TestLoadSecretsRejectsOpenPermissions(t *testing.T) {
	p := write(t, ".secrets", "switches: {}\n", 0o644)
	_, err := LoadSecrets(p)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("err = %v, want a permissions error", err)
	}
}

func TestLoadSecretsMissingIsEmpty(t *testing.T) {
	s, err := LoadSecrets(filepath.Join(t.TempDir(), ".secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Switches) != 0 {
		t.Errorf("secrets = %+v, want empty", s)
	}
}

func TestSecretErrorsDoNotLeakValues(t *testing.T) {
	p := write(t, ".secrets", "switches:\n  lab:\n    pasword: hunter2\n", 0o600)
	_, err := LoadSecrets(p)
	if err == nil {
		t.Fatal("want error for unknown field")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the secret: %v", err)
	}
}

func TestLoadSiteNodes(t *testing.T) {
	p := write(t, "site.yaml", `
nodes:
  - name: k53
    console:
      device: /dev/serial/by-id/usb-probe
  - name: k52
    console: {device: /dev/ttyUSB0, baud: 9600}
  - name: k41
`, 0o644)
	s, err := LoadSite(p)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Node("k53")
	if err != nil {
		t.Fatal(err)
	}
	if n.Console == nil || n.Console.Device != "/dev/serial/by-id/usb-probe" || n.Console.Baud != 115200 {
		t.Errorf("k53 = %+v (baud should default to 115200)", n.Console)
	}
	if n, _ := s.Node("k52"); n.Console.Baud != 9600 {
		t.Errorf("k52 baud = %d", n.Console.Baud)
	}
	if n, _ := s.Node("k41"); n.Console != nil {
		t.Errorf("k41 has no console, got %+v", n.Console)
	}
	if _, err := s.Node("k99"); err == nil {
		t.Error("unknown node: want error")
	}
}

func TestLoadSiteNodeErrors(t *testing.T) {
	tests := map[string]string{
		"no name":           "nodes:\n  - console: {device: /dev/x}\n",
		"duplicate":         "nodes:\n  - name: a\n  - name: a\n",
		"console no device": "nodes:\n  - name: a\n    console: {baud: 9600}\n",
		"negative baud":     "nodes:\n  - name: a\n    console: {device: /dev/x, baud: -1}\n",
	}
	for name, body := range tests {
		if _, err := LoadSite(write(t, "site.yaml", body, 0o644)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadSiteNetworks(t *testing.T) {
	p := write(t, "site.yaml", `
networks:
  - name: pinet
    interface: eth0.3
    address: 10.0.3.1/24
    pool: 10.0.3.100-10.0.3.199
`, 0o644)
	s, err := LoadSite(p)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Network("pinet")
	if err != nil {
		t.Fatal(err)
	}
	if n.Interface != "eth0.3" || n.Server != netip.MustParseAddr("10.0.3.1") ||
		n.Subnet != netip.MustParsePrefix("10.0.3.0/24") ||
		n.PoolFirst != netip.MustParseAddr("10.0.3.100") || n.PoolLast != netip.MustParseAddr("10.0.3.199") {
		t.Errorf("network = %+v", n)
	}
	if _, err := s.Network("nope"); err == nil {
		t.Error("unknown network: want error")
	}
}

func TestLoadSiteNetworkErrors(t *testing.T) {
	net := func(fields string) string {
		return "networks:\n  - {name: pinet, interface: eth0.3, " + fields + "}\n"
	}
	tests := map[string]string{
		"no name":            "networks:\n  - {interface: eth0.3, address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199}\n",
		"no interface":       "networks:\n  - {name: pinet, address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199}\n",
		"duplicate":          net("address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199") + "  - {name: pinet, interface: eth0.4, address: 10.0.4.1/24, pool: 10.0.4.100-10.0.4.199}\n",
		"no prefix length":   net("address: 10.0.3.1, pool: 10.0.3.100-10.0.3.199"),
		"IPv6 address":       net("address: fd00::1/64, pool: 10.0.3.100-10.0.3.199"),
		"pool not a range":   net("address: 10.0.3.1/24, pool: 10.0.3.100"),
		"pool reversed":      net("address: 10.0.3.1/24, pool: 10.0.3.199-10.0.3.100"),
		"pool outside":       net("address: 10.0.3.1/24, pool: 10.0.4.100-10.0.4.199"),
		"pool holds server":  net("address: 10.0.3.1/24, pool: 10.0.3.1-10.0.3.199"),
		"pool holds network": net("address: 10.0.3.1/24, pool: 10.0.3.0-10.0.3.9"),
		"pool holds bcast":   net("address: 10.0.3.1/24, pool: 10.0.3.250-10.0.3.255"),
	}
	for name, body := range tests {
		if _, err := LoadSite(write(t, "site.yaml", body, 0o644)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadSiteInstallFields(t *testing.T) {
	p := write(t, "site.yaml", `
nodes:
  - {name: k52, disk: mmcblk0}
networks:
  - {name: pinet, interface: eth0.3, address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199, vlan: pinet}
`, 0o644)
	s, err := LoadSite(p)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Node("k52"); n.Disk != "mmcblk0" {
		t.Errorf("disk = %q", n.Disk)
	}
	if n, _ := s.Network("pinet"); n.VLAN != "pinet" {
		t.Errorf("vlan = %q", n.VLAN)
	}
	for name, body := range map[string]string{
		"bad disk": "nodes:\n  - {name: k52, disk: /dev/mmcblk0}\n",
	} {
		if _, err := LoadSite(write(t, "site.yaml", body, 0o644)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadSiteFirstBoot(t *testing.T) {
	p := write(t, "site.yaml", `
sshKeys:
  - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample stage0@pilab
nodes:
  - {name: k52}
  - {name: k400, hostname: head}
`, 0o644)
	s, err := LoadSite(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SSHKeys) != 1 {
		t.Errorf("keys %v", s.SSHKeys)
	}
	if n, _ := s.Node("k52"); n.Hostname != "k52" {
		t.Errorf("k52 hostname %q, want its name by default", n.Hostname)
	}
	if n, _ := s.Node("k400"); n.Hostname != "head" {
		t.Errorf("k400 hostname %q", n.Hostname)
	}
	for name, body := range map[string]string{
		"private key":  "sshKeys: ['-----BEGIN OPENSSH PRIVATE KEY-----']\n",
		"not a key":    "sshKeys: ['hello']\n",
		"multi-line":   "sshKeys: [\"ssh-ed25519 AAAA x\\nssh-rsa BBBB y\"]\n",
		"bad hostname": "nodes:\n  - {name: k52, hostname: 'k 52'}\n",
	} {
		if _, err := LoadSite(write(t, "site.yaml", body, 0o644)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadSitePrefix6(t *testing.T) {
	ok := "networks:\n  - {name: pinet, interface: eth0.3, address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199, prefix6: 'fd00:10:3::/64'}\n"
	s, err := LoadSite(write(t, "site.yaml", ok, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Network("pinet"); n.IPv6Prefix.String() != "fd00:10:3::/64" {
		t.Errorf("prefix %v", n.IPv6Prefix)
	}
	for _, bad := range []string{"fd00:10:3::/48", "10.0.3.0/24", "nonsense"} {
		body := "networks:\n  - {name: pinet, interface: eth0.3, address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199, prefix6: '" + bad + "'}\n"
		if _, err := LoadSite(write(t, "site.yaml", body, 0o644)); err == nil {
			t.Errorf("prefix6 %s: want error", bad)
		}
	}
}
