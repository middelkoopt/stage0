// Package osconfig is the installed system's own configuration that stage0
// writes on the container route when it configures first boot itself
// (docs/firstboot.md): the network, as systemd-networkd or NetworkManager
// files, the hostname and root's SSH keys. Render turns it into files;
// the agent writes them into the installed root.
package osconfig

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// The network configuration tools Render writes for.
const (
	Networkd       = "networkd"
	NetworkManager = "networkmanager"
)

// System is what stage0 configures in the installed system.
type System struct {
	Hostname string   `json:"hostname"`
	Keys     []string `json:"keys,omitempty"` // root's SSH keys
	// Network is the image's network tool: Networkd or NetworkManager.
	Network string `json:"network"`
	// Interfaces; none means DHCP (and SLAAC) on the NIC the agent booted
	// on.
	Interfaces []Interface `json:"interfaces,omitempty"`
}

// Interface is one Ethernet NIC, by MAC; with no MAC, the NIC the agent
// booted on (at most one such).
type Interface struct {
	MAC      string `yaml:"mac,omitempty" json:"mac,omitempty"`
	Settings `yaml:",inline"`
	VLANs    []VLAN `yaml:"vlans,omitempty" json:"vlans,omitempty"` // tagged, on top
}

// VLAN is a tagged VLAN on an interface, named vlan<ID> on the node.
type VLAN struct {
	ID       int `yaml:"id" json:"id"`
	Settings `yaml:",inline"`
}

// Settings are an interface's addresses. DHCP means DHCPv4 and IPv6
// autoconfiguration (router advertisements, DHCPv6 when they say so);
// without it only Addresses, and no autoconfiguration. Neither: the link
// only carries VLANs.
type Settings struct {
	DHCP      bool     `yaml:"dhcp,omitempty" json:"dhcp,omitempty"`
	Addresses []string `yaml:"addresses,omitempty" json:"addresses,omitempty"` // with prefix, e.g. 10.0.23.5/24
	Gateway   string   `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	DNS       []string `yaml:"dns,omitempty" json:"dns,omitempty"`
}

// Check checks interfaces as given in site.yaml.
func Check(ifs []Interface) error {
	bootNIC := false
	macs := map[string]bool{}
	for i, f := range ifs {
		what := fmt.Sprintf("interface %d", i+1)
		if f.MAC == "" {
			if bootNIC {
				return fmt.Errorf("%s: only one interface may leave out mac (the NIC it boots on)", what)
			}
			bootNIC = true
		} else {
			hw, err := net.ParseMAC(f.MAC)
			if err != nil || len(hw) != 6 {
				return fmt.Errorf("%s: mac %q is not like d8:3a:dd:c5:c1:f3", what, f.MAC)
			}
			if macs[hw.String()] {
				return fmt.Errorf("%s: mac %s twice", what, hw)
			}
			macs[hw.String()] = true
		}
		if err := f.Settings.check(what); err != nil {
			return err
		}
		ids := map[int]bool{}
		for _, v := range f.VLANs {
			vw := fmt.Sprintf("%s, vlan %d", what, v.ID)
			switch {
			case v.ID < 1 || v.ID > 4094:
				return fmt.Errorf("%s: id must be 1-4094", vw)
			case ids[v.ID]:
				return fmt.Errorf("%s: twice", vw)
			}
			ids[v.ID] = true
			if err := v.Settings.check(vw); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s Settings) check(what string) error {
	for _, a := range s.Addresses {
		if _, err := netip.ParsePrefix(a); err != nil {
			return fmt.Errorf("%s: address %q is not like 10.0.23.5/24", what, a)
		}
	}
	if s.Gateway != "" {
		if _, err := netip.ParseAddr(s.Gateway); err != nil {
			return fmt.Errorf("%s: gateway %q is not an address", what, s.Gateway)
		}
	}
	for _, d := range s.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			return fmt.Errorf("%s: dns %q is not an address", what, d)
		}
	}
	if !s.DHCP && len(s.Addresses) == 0 && (s.Gateway != "" || len(s.DNS) > 0) {
		return fmt.Errorf("%s: gateway or dns without dhcp or addresses", what)
	}
	return nil
}

// File is a file to write in the installed root.
type File struct {
	Path string // absolute, in the installed system
	Mode uint32
	Data string
}

// Render returns the files for sys; bootMAC is the NIC the agent booted
// on, for the interface without a MAC.
func Render(sys System, bootMAC string) ([]File, error) {
	if sys.Hostname == "" {
		return nil, errors.New("no hostname")
	}
	if err := Check(sys.Interfaces); err != nil {
		return nil, err
	}
	ifs := sys.Interfaces
	if len(ifs) == 0 {
		ifs = []Interface{{Settings: Settings{DHCP: true}}}
	}
	for i := range ifs {
		if ifs[i].MAC == "" {
			if bootMAC == "" {
				return nil, errors.New("no MAC for the boot NIC")
			}
			ifs[i].MAC = bootMAC
		}
		hw, _ := net.ParseMAC(ifs[i].MAC)
		ifs[i].MAC = hw.String()
	}
	files := []File{{Path: "/etc/hostname", Mode: 0o644, Data: sys.Hostname + "\n"}}
	if len(sys.Keys) > 0 {
		files = append(files, File{Path: "/root/.ssh/authorized_keys", Mode: 0o600, Data: strings.Join(sys.Keys, "\n") + "\n"})
	}
	switch sys.Network {
	case Networkd:
		files = append(files, networkd(ifs)...)
	case NetworkManager:
		files = append(files, networkManager(ifs)...)
	default:
		return nil, fmt.Errorf("network %q: want %s or %s", sys.Network, Networkd, NetworkManager)
	}
	return files, nil
}

// networkd writes 10-stage0-* files, so they come before the image's own.
func networkd(ifs []Interface) []File {
	const dir = "/etc/systemd/network/"
	var files []File
	for i, f := range ifs {
		name := fmt.Sprintf("10-stage0-eth%d", i)
		var b strings.Builder
		// Type=ether: a VLAN on the NIC has the NIC's MAC too.
		fmt.Fprintf(&b, "[Match]\nMACAddress=%s\nType=ether\n\n[Network]\n", f.MAC)
		networkdSettings(&b, f.Settings)
		for _, v := range f.VLANs {
			fmt.Fprintf(&b, "VLAN=vlan%d\n", v.ID)
		}
		files = append(files, File{Path: dir + name + ".network", Mode: 0o644, Data: b.String()})
		for _, v := range f.VLANs {
			vname := fmt.Sprintf("10-stage0-vlan%d", v.ID)
			files = append(files, File{Path: dir + vname + ".netdev", Mode: 0o644,
				Data: fmt.Sprintf("[NetDev]\nName=vlan%d\nKind=vlan\n\n[VLAN]\nId=%d\n", v.ID, v.ID)})
			var vb strings.Builder
			fmt.Fprintf(&vb, "[Match]\nName=vlan%d\n\n[Network]\n", v.ID)
			networkdSettings(&vb, v.Settings)
			files = append(files, File{Path: dir + vname + ".network", Mode: 0o644, Data: vb.String()})
		}
	}
	return files
}

func networkdSettings(b *strings.Builder, s Settings) {
	if s.DHCP {
		b.WriteString("DHCP=yes\n")
	} else {
		b.WriteString("DHCP=no\nIPv6AcceptRA=no\n")
		if len(s.Addresses) == 0 {
			b.WriteString("LinkLocalAddressing=no\n")
		}
	}
	for _, a := range s.Addresses {
		fmt.Fprintf(b, "Address=%s\n", a)
	}
	if s.Gateway != "" {
		fmt.Fprintf(b, "Gateway=%s\n", s.Gateway)
	}
	for _, d := range s.DNS {
		fmt.Fprintf(b, "DNS=%s\n", d)
	}
}

// networkManager writes keyfiles (mode 600, or NetworkManager ignores
// them).
func networkManager(ifs []Interface) []File {
	const dir = "/etc/NetworkManager/system-connections/"
	var files []File
	for i, f := range ifs {
		id := fmt.Sprintf("stage0-eth%d", i)
		uuid := uuidFor(id + f.MAC)
		var b strings.Builder
		fmt.Fprintf(&b, "[connection]\nid=%s\nuuid=%s\ntype=ethernet\nautoconnect=true\n\n[ethernet]\nmac-address=%s\n\n", id, uuid, f.MAC)
		nmSettings(&b, f.Settings)
		files = append(files, File{Path: dir + id + ".nmconnection", Mode: 0o600, Data: b.String()})
		for _, v := range f.VLANs {
			vid := fmt.Sprintf("stage0-vlan%d", v.ID)
			var vb strings.Builder
			fmt.Fprintf(&vb, "[connection]\nid=%s\nuuid=%s\ntype=vlan\ninterface-name=vlan%d\nautoconnect=true\n\n[vlan]\nid=%d\nparent=%s\n\n",
				vid, uuidFor(vid+f.MAC), v.ID, v.ID, uuid)
			nmSettings(&vb, v.Settings)
			files = append(files, File{Path: dir + vid + ".nmconnection", Mode: 0o600, Data: vb.String()})
		}
	}
	return files
}

func nmSettings(b *strings.Builder, s Settings) {
	for _, family := range []struct {
		name string
		is4  bool
	}{{"ipv4", true}, {"ipv6", false}} {
		var addrs, dns []string
		for _, a := range s.Addresses {
			if p := netip.MustParsePrefix(a); p.Addr().Is4() == family.is4 {
				addrs = append(addrs, a)
			}
		}
		for _, d := range s.DNS {
			if netip.MustParseAddr(d).Is4() == family.is4 {
				dns = append(dns, d)
			}
		}
		method := "disabled"
		switch {
		case s.DHCP:
			method = "auto"
		case len(addrs) > 0:
			method = "manual"
		case !family.is4:
			method = "link-local"
			if len(s.Addresses) == 0 {
				method = "disabled" // a link that only carries VLANs
			}
		}
		fmt.Fprintf(b, "[%s]\nmethod=%s\n", family.name, method)
		for i, a := range addrs {
			fmt.Fprintf(b, "address%d=%s\n", i+1, a)
		}
		if g := s.Gateway; g != "" && netip.MustParseAddr(g).Is4() == family.is4 {
			fmt.Fprintf(b, "gateway=%s\n", g)
		}
		if len(dns) > 0 {
			fmt.Fprintf(b, "dns=%s;\n", strings.Join(dns, ";"))
		}
		b.WriteString("\n")
	}
}

// uuidFor is a fixed UUID (version 8, from SHA-256) for a connection, so
// the same node gets the same files every install.
func uuidFor(name string) string {
	h := sha256.Sum256([]byte("stage0/" + name))
	h[6] = h[6]&0x0f | 0x80
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Paths lists the files' paths, sorted, for reports.
func Paths(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	slices.Sort(out)
	return out
}
