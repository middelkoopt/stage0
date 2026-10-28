// Package config loads the site configuration (site.yaml, committed) and the
// credentials that go with it (.secrets, git-ignored). Every field has a
// default where one makes sense; unknown fields are errors, so a typo fails
// loudly instead of being silently ignored.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/middelkoopt/stage0/internal/container"
	"github.com/middelkoopt/stage0/internal/image"
)

// Default file names, relative to the working directory.
const (
	DefaultSitePath    = "site.yaml"
	DefaultSecretsPath = ".secrets"
)

// Site is site.yaml.
type Site struct {
	Switches []Switch  `yaml:"switches"`
	Nodes    []Node    `yaml:"nodes"`
	Networks []Network `yaml:"networks"`

	// SSHKeys are public keys allowed to log in to installed nodes as root.
	SSHKeys []string `yaml:"sshKeys"`
}

// Switch is a managed switch.
type Switch struct {
	Name    string `yaml:"name"`             // key into .secrets
	Driver  string `yaml:"driver,omitempty"` // default "gs316ep"
	Address string `yaml:"address"`          // host name or URL
}

// Node is a machine stage0 manages. Its name is its switch port's label.
type Node struct {
	Name    string   `yaml:"name"`
	Console *Console `yaml:"console,omitempty"`
	Disk    string   `yaml:"disk,omitempty"` // install target under /dev, e.g. mmcblk0

	// Hostname the installed system gets; default the node's name.
	Hostname string `yaml:"hostname,omitempty"`

	// Network it is installed over; default the only one in the site.
	Network string `yaml:"network,omitempty"`

	// VM makes the node a QEMU VM on the test bed instead of a machine on
	// a switch port; its console is QEMU's serial.
	VM *VM `yaml:"vm,omitempty"`

	// Partitions, for installing a container, override the image config's
	// and the container's (docs/containers.md): this node's disk may differ.
	Partitions []container.Partition `yaml:"partitions,omitempty"`

	// Ignition, instead of Partitions, is an Ignition config (JSON, a path
	// relative to site.yaml) whose storage section gives the layout.
	Ignition       string `yaml:"ignition,omitempty"`
	IgnitionConfig []byte `yaml:"-"` // its contents

	// FirstBoot and CloudInit override the image config's (docs/firstboot.md).
	FirstBoot string          `yaml:"firstBoot,omitempty"`
	CloudInit image.CloudInit `yaml:"cloudInit,omitempty"`

	// BMC makes `stage0 bmc` answer IPMI for the node (docs/bmc.md).
	BMC *BMC `yaml:"bmc,omitempty"`
}

// BMC is a node's virtual BMC: an IPMI 2.0 endpoint on its own UDP port,
// power backed by its PoE port, SOL by its serial console. The user and
// password are in .secrets (bmc:).
type BMC struct {
	Port int `yaml:"port"`
}

// VM is a test bed VM's hardware.
type VM struct {
	MAC      string `yaml:"mac"`                // its NIC's address
	Memory   int    `yaml:"memory,omitempty"`   // MB, default 2048
	CPUs     int    `yaml:"cpus,omitempty"`     // default 2
	DiskGB   int    `yaml:"diskGB,omitempty"`   // default 16
	Firmware string `yaml:"firmware,omitempty"` // UEFI code (AAVMF_VARS.fd beside it); default Ubuntu's, fetched by stage0
}

// Console is a node's local serial console.
type Console struct {
	Device string `yaml:"device"`         // prefer the stable /dev/serial/by-id/... name
	Baud   int    `yaml:"baud,omitempty"` // default 115200
}

// Network is a provisioning network stage0 serves (DHCPv4 and TFTP). The
// host's interface already carries Address; stage0 doesn't configure it.
type Network struct {
	Name      string `yaml:"name"`
	Interface string `yaml:"interface"`         // e.g. eth0.3
	Address   string `yaml:"address"`           // stage0's IPv4 address with prefix, e.g. 10.0.3.1/24
	Pool      string `yaml:"pool"`              // addresses to lease: first-last
	VLAN      string `yaml:"vlan,omitempty"`    // the switch VLAN (name or ID) nodes must be on to install
	Prefix6   string `yaml:"prefix6,omitempty"` // IPv6 /64 to advertise for SLAAC, e.g. fd00:10:3::/64
	// Mode: "own" (default: stage0 hands out the addresses) or "proxy"
	// (another DHCP server does; stage0 answers boot requests only, as
	// ProxyDHCP, and needs no pool). docs/network-modes.md.
	Mode string `yaml:"mode,omitempty"`
	// NTP servers (IPv4 addresses) DHCP hands out: installed Pis have no
	// clock battery. stage0 doesn't serve NTP; e.g. chrony on its host.
	NTP []string `yaml:"ntp,omitempty"`

	// Parsed from the fields above.
	Server              netip.Addr   `yaml:"-"`
	Subnet              netip.Prefix `yaml:"-"`
	PoolFirst, PoolLast netip.Addr   `yaml:"-"`
	IPv6Prefix          netip.Prefix `yaml:"-"` // from Prefix6; invalid if none
	Server6             netip.Addr   `yaml:"-"` // <prefix>::1, stage0's IPv6 address on it
	NTPServers          []netip.Addr `yaml:"-"` // from NTP
}

var (
	diskName   = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	hostname   = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	sshKeyType = regexp.MustCompile(`^(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp(256|384|521)|sk-[a-z0-9@.-]+)$`)
)

// Drivers are the switch drivers stage0 knows.
var Drivers = []string{"gs316ep"}

// LoadSite reads and validates a site file.
func LoadSite(path string) (*Site, error) {
	var s Site
	if err := decode(path, &s); err != nil {
		return nil, err
	}
	if err := s.normalize(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// normalize checks the site and fills in defaults; dir is the site
// file's directory, which relative paths in it are under.
func (s *Site) normalize(dir string) error {
	seen := map[string]bool{}
	for i := range s.Switches {
		sw := &s.Switches[i]
		switch {
		case sw.Name == "":
			return fmt.Errorf("switch %d: no name", i+1)
		case seen[sw.Name]:
			return fmt.Errorf("switch %q: defined twice", sw.Name)
		case sw.Address == "":
			return fmt.Errorf("switch %q: no address", sw.Name)
		}
		seen[sw.Name] = true
		if sw.Driver == "" {
			sw.Driver = Drivers[0]
		}
		if !slices.Contains(Drivers, sw.Driver) {
			return fmt.Errorf("switch %q: unknown driver %q (known: %v)", sw.Name, sw.Driver, Drivers)
		}
	}

	seen = map[string]bool{}
	bmcPorts := map[int]string{}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		switch {
		case n.Name == "":
			return fmt.Errorf("node %d: no name", i+1)
		case seen[n.Name]:
			return fmt.Errorf("node %q: defined twice", n.Name)
		}
		seen[n.Name] = true
		if n.Hostname == "" {
			n.Hostname = n.Name
		}
		if !hostname.MatchString(n.Hostname) {
			return fmt.Errorf("node %q: hostname %q is not a valid host name", n.Name, n.Hostname)
		}
		if v := n.VM; v != nil {
			hw, err := net.ParseMAC(v.MAC)
			switch {
			case err != nil || len(hw) != 6:
				return fmt.Errorf("node %q: vm needs a mac like 52:54:00:12:34:56", n.Name)
			case n.Console != nil:
				return fmt.Errorf("node %q: a VM's console is QEMU's serial; drop console", n.Name)
			}
			v.MAC = hw.String()
			v.Memory = cmp.Or(v.Memory, 2048)
			v.CPUs = cmp.Or(v.CPUs, 2)
			v.DiskGB = cmp.Or(v.DiskGB, 16)
		}
		if b := n.BMC; b != nil {
			switch {
			case b.Port < 1 || b.Port > 65535:
				return fmt.Errorf("node %q: bmc port %d", n.Name, b.Port)
			case bmcPorts[b.Port] != "":
				return fmt.Errorf("node %q: bmc port %d is %s's too", n.Name, b.Port, bmcPorts[b.Port])
			}
			bmcPorts[b.Port] = n.Name
		}
		if err := image.CheckFirstBoot(n.FirstBoot, n.CloudInit); err != nil {
			return fmt.Errorf("node %q: %w", n.Name, err)
		}
		if err := n.CloudInit.Load(dir); err != nil {
			return fmt.Errorf("node %q: %w", n.Name, err)
		}
		if n.Ignition != "" {
			if len(n.Partitions) > 0 {
				return fmt.Errorf("node %q: partitions or ignition, not both", n.Name)
			}
			p := n.Ignition
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("node %q: %w", n.Name, err)
			}
			if n.Partitions, err = container.FromIgnition(b); err != nil {
				return fmt.Errorf("node %q: %w", n.Name, err)
			}
			n.IgnitionConfig = b
		}
		if len(n.Partitions) > 0 {
			if err := container.CheckPartitions(n.Partitions); err != nil {
				return fmt.Errorf("node %q: %w", n.Name, err)
			}
		}
		if n.Disk != "" && !diskName.MatchString(n.Disk) {
			return fmt.Errorf("node %q: disk %q is not a name under /dev like mmcblk0", n.Name, n.Disk)
		}
		if c := n.Console; c != nil {
			switch {
			case c.Device == "":
				return fmt.Errorf("node %q: console has no device", n.Name)
			case c.Baud < 0:
				return fmt.Errorf("node %q: console baud %d", n.Name, c.Baud)
			case c.Baud == 0:
				c.Baud = 115200
			}
		}
	}

	for _, n := range s.Nodes {
		if n.Network != "" {
			if _, err := s.Network(n.Network); err != nil {
				return fmt.Errorf("node %q: %w", n.Name, err)
			}
		}
	}
	for i, k := range s.SSHKeys {
		f := strings.Fields(k)
		if strings.ContainsAny(k, "\r\n") || len(f) < 2 || !sshKeyType.MatchString(f[0]) {
			return fmt.Errorf("sshKeys %d: not a one-line public key (\"ssh-ed25519 AAAA... comment\")", i+1)
		}
	}

	seen = map[string]bool{}
	for i := range s.Networks {
		n := &s.Networks[i]
		switch {
		case n.Name == "":
			return fmt.Errorf("network %d: no name", i+1)
		case seen[n.Name]:
			return fmt.Errorf("network %q: defined twice", n.Name)
		case n.Interface == "":
			return fmt.Errorf("network %q: no interface", n.Name)
		}
		seen[n.Name] = true
		if err := n.parse(); err != nil {
			return fmt.Errorf("network %q: %w", n.Name, err)
		}
	}
	return nil
}

func (n *Network) parse() error {
	p, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return fmt.Errorf("address %q: want an IPv4 address with prefix length, e.g. 10.0.3.1/24", n.Address)
	}
	if !p.Addr().Is4() {
		return fmt.Errorf("address %q: only IPv4 is served so far", n.Address)
	}
	n.Server, n.Subnet = p.Addr(), p.Masked()

	for _, a := range n.NTP {
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.Is4() {
			return fmt.Errorf("ntp %q: want an IPv4 address", a)
		}
		n.NTPServers = append(n.NTPServers, ip)
	}
	if n.Prefix6 != "" {
		p6, err := netip.ParsePrefix(n.Prefix6)
		if err != nil || !p6.Addr().Is6() || p6.Bits() != 64 {
			return fmt.Errorf("prefix6 %q: want an IPv6 /64, e.g. fd00:10:3::/64 (SLAAC needs a /64)", n.Prefix6)
		}
		n.IPv6Prefix = p6.Masked()
		a := n.IPv6Prefix.Addr().As16()
		a[15] = 1
		n.Server6 = netip.AddrFrom16(a)
	}
	switch n.Mode {
	case "":
		n.Mode = "own"
	case "own", "proxy":
	default:
		return fmt.Errorf("mode %q: own or proxy", n.Mode)
	}
	if n.Mode == "proxy" {
		if n.Pool != "" {
			return errors.New("pool: in proxy mode the other DHCP server hands out addresses")
		}
		return nil
	}
	first, last, ok := strings.Cut(n.Pool, "-")
	if !ok {
		return fmt.Errorf("pool %q: want first-last, e.g. 10.0.3.100-10.0.3.199", n.Pool)
	}
	if n.PoolFirst, err = netip.ParseAddr(strings.TrimSpace(first)); err != nil {
		return fmt.Errorf("pool %q: %w", n.Pool, err)
	}
	if n.PoolLast, err = netip.ParseAddr(strings.TrimSpace(last)); err != nil {
		return fmt.Errorf("pool %q: %w", n.Pool, err)
	}
	broadcast := lastAddr(n.Subnet)
	switch {
	case n.PoolLast.Less(n.PoolFirst):
		return fmt.Errorf("pool %q: last is before first", n.Pool)
	case !n.Subnet.Contains(n.PoolFirst) || !n.Subnet.Contains(n.PoolLast):
		return fmt.Errorf("pool %q is not inside %s", n.Pool, n.Subnet)
	case n.PoolFirst == n.Subnet.Addr() || n.PoolLast == broadcast:
		return fmt.Errorf("pool %q includes the network or broadcast address", n.Pool)
	case !n.Server.Less(n.PoolFirst) && !n.PoolLast.Less(n.Server):
		return fmt.Errorf("pool %q includes stage0's own address %s", n.Pool, n.Server)
	}
	return nil
}

// lastAddr is the last (broadcast) address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Network returns the named network.
func (s *Site) Network(name string) (Network, error) {
	for _, n := range s.Networks {
		if n.Name == name {
			return n, nil
		}
	}
	return Network{}, fmt.Errorf("no network %q in site config", name)
}

// Node returns the named node.
func (s *Site) Node(name string) (Node, error) {
	for _, n := range s.Nodes {
		if n.Name == name {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("no node %q in site config", name)
}

// Switch returns the named switch, or the only one when name is empty.
func (s *Site) Switch(name string) (Switch, error) {
	if name == "" {
		switch len(s.Switches) {
		case 0:
			return Switch{}, errors.New("no switches in site config")
		case 1:
			return s.Switches[0], nil
		default:
			return Switch{}, errors.New("several switches in site config; name one")
		}
	}
	for _, sw := range s.Switches {
		if sw.Name == name {
			return sw, nil
		}
	}
	return Switch{}, fmt.Errorf("no switch %q in site config", name)
}

// Secrets is .secrets.
type Secrets struct {
	Switches map[string]SwitchSecret `yaml:"switches"`
	// BMC is the login of the nodes' virtual BMCs (stage0 bmc).
	BMC *BMCSecret `yaml:"bmc,omitempty"`
}

// BMCSecret is an IPMI user; the password at most 20 bytes (IPMI 2.0).
type BMCSecret struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// SwitchSecret holds one switch's credentials: Password, or LoginHash, a
// precomputed login value from the older lab switch.py.
type SwitchSecret struct {
	Password  string `yaml:"password,omitempty"`
	LoginHash string `yaml:"loginHash,omitempty"`
}

// LoadSecrets reads the secrets file. A missing file means no secrets; a file
// that group or others can read is refused.
func LoadSecrets(path string) (*Secrets, error) {
	s := &Secrets{Switches: map[string]SwitchSecret{}}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by group or others; run: chmod 600 %s", path, path)
	}
	if err := decode(path, s); err != nil {
		return nil, err
	}
	return s, nil
}

func decode(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
