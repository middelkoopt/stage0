package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"

	"github.com/middelkoopt/stage0/internal/dhcpd"
)

// bringUpNetwork gives the agent an IPv4 address if the kernel didn't
// (ip=dhcp works only when the network driver is built in; a VM's EL
// kernel loads virtio_net as a module, after the kernel's own DHCP): each
// Ethernet interface is brought up and asks stage0 by DHCP until one gets
// a lease.
func bringUpNetwork(out io.Writer, ipv6 bool) error {
	if ipv6 {
		return bringUpIPv6(out)
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	var candidates []net.Interface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) != 6 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
				return nil // the kernel's ip=dhcp did it
			}
		}
		candidates = append(candidates, ifc)
	}
	if len(candidates) == 0 {
		return errors.New("no Ethernet interface")
	}
	var errs []error
	for _, ifc := range candidates {
		if err := setLinkUp(ifc.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		ip, err := dhcp(ifc.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ifc.Name, err))
			continue
		}
		if err := addAddr(ifc.Index, ip); err != nil {
			return fmt.Errorf("%s: %w", ifc.Name, err)
		}
		fmt.Fprintf(out, "stage0 agent: %s has %s from DHCP\n", ifc.Name, ip)
		return nil
	}
	return errors.Join(errs...)
}

// dhcp asks for an address on ifname, as stage0's agent.
func dhcp(ifname string) (netip.Prefix, error) {
	c, err := nclient4.New(ifname, nclient4.WithTimeout(3*time.Second), nclient4.WithRetry(5))
	if err != nil {
		return netip.Prefix{}, err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := c.Request(ctx, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(dhcpd.AgentClass)))
	if err != nil {
		return netip.Prefix{}, err
	}
	ip, ok := netip.AddrFromSlice(lease.ACK.YourIPAddr.To4())
	if !ok {
		return netip.Prefix{}, errors.New("lease without an IPv4 address")
	}
	bits, _ := net.IPMask(lease.ACK.SubnetMask()).Size()
	if bits == 0 {
		bits = 24
	}
	return netip.PrefixFrom(ip, bits), nil
}

// setLinkUp sets an interface's IFF_UP flag.
func setLinkUp(name string) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var req [40]byte // struct ifreq: name, then flags
	copy(req[:15], name)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req[0]))); e != 0 {
		return fmt.Errorf("%s: get flags: %w", name, e)
	}
	flags := binary.NativeEndian.Uint16(req[16:]) | syscall.IFF_UP
	binary.NativeEndian.PutUint16(req[16:], flags)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req[0]))); e != 0 {
		return fmt.Errorf("%s: set up: %w", name, e)
	}
	return nil
}

// newAddrMessage is an RTM_NEWADDR request adding p to interface index.
func newAddrMessage(index int, p netip.Prefix, seq uint32) []byte {
	a := p.Addr().As4()
	b := make([]byte, 40)
	binary.NativeEndian.PutUint32(b[0:], 40)
	binary.NativeEndian.PutUint16(b[4:], syscall.RTM_NEWADDR)
	binary.NativeEndian.PutUint16(b[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_EXCL|syscall.NLM_F_CREATE)
	binary.NativeEndian.PutUint32(b[8:], seq)
	b[16], b[17] = syscall.AF_INET, byte(p.Bits()) // ifaddrmsg
	binary.NativeEndian.PutUint32(b[20:], uint32(index))
	for i, typ := range []uint16{syscall.IFA_LOCAL, syscall.IFA_ADDRESS} {
		o := 24 + 8*i
		binary.NativeEndian.PutUint16(b[o:], 8)
		binary.NativeEndian.PutUint16(b[o+2:], typ)
		copy(b[o+4:], a[:])
	}
	return b
}

// addAddr adds p to interface index over netlink and waits for the ack.
func addAddr(index int, p netip.Prefix) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Sendto(fd, newAddrMessage(index, p, 1), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink: %w", err)
	}
	buf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return fmt.Errorf("netlink: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return fmt.Errorf("netlink: %w", err)
	}
	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR && len(m.Data) >= 4 {
			if code := int32(binary.NativeEndian.Uint32(m.Data)); code != 0 {
				return fmt.Errorf("adding %s: %w", p, syscall.Errno(-code))
			}
		}
	}
	return nil
}

// wantsIPv6 reports whether the command line's image URL is an IPv6
// literal: the install then runs over IPv6 alone.
func wantsIPv6(cmdline string) bool {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, "stage0.url="); ok {
			return strings.HasPrefix(v, "http://[")
		}
	}
	return false
}

// firstGlobal6 returns the first global IPv6 address among addrs.
func firstGlobal6(addrs []netip.Prefix) (netip.Addr, bool) {
	for _, p := range addrs {
		if a := p.Addr(); a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// bringUpIPv6 brings every Ethernet interface up and waits for the kernel
// to form a global address from a router advertisement (SLAAC).
func bringUpIPv6(out io.Writer) error {
	// A distro kernel may build IPv6 as a module (Raspberry Pi OS and
	// Rocky's Pi kernels do).
	if _, err := os.Stat("/proc/net/if_inet6"); err != nil {
		if mp := findModprobe("/"); mp != "" {
			if b, err := exec.Command(mp, "ipv6").CombinedOutput(); err != nil {
				fmt.Fprintf(out, "stage0 agent: modprobe ipv6: %v %s\n", err, b)
			}
		}
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback == 0 && len(ifc.HardwareAddr) == 6 {
			if err := setLinkUp(ifc.Name); err != nil {
				fmt.Fprintf(out, "stage0 agent: %v\n", err)
			}
		}
	}
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		ifs, _ := net.Interfaces()
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := ifc.Addrs()
			var ps []netip.Prefix
			for _, a := range addrs {
				if p, err := netip.ParsePrefix(a.String()); err == nil {
					ps = append(ps, p)
				}
			}
			if a, ok := firstGlobal6(ps); ok {
				fmt.Fprintf(out, "stage0 agent: %s has %s from a router advertisement\n", ifc.Name, a)
				return nil
			}
		}
	}
	return errors.New("no IPv6 address from a router advertisement within 60 s")
}
