package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/middelkoopt/stage0/internal/osconfig"
)

// configureSystem writes the installed system's own configuration
// (docs/firstboot.md) into root: its network for the image's tool, its
// hostname and root's keys. It also makes the node its own: an image's
// machine ID and SSH host keys would otherwise be every node's.
func configureSystem(out io.Writer, root string, sys osconfig.System, server string) ([]string, error) {
	mac, err := findBootMAC(server)
	if err != nil {
		return nil, fmt.Errorf("finding the NIC it booted on: %w", err)
	}
	files, err := osconfig.Render(sys, mac)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		p := filepath.Join(root, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte(f.Data), fs.FileMode(f.Mode)); err != nil {
			return nil, err
		}
		if err := os.Chmod(p, fs.FileMode(f.Mode)); err != nil { // WriteFile keeps an old file's mode
			return nil, err
		}
	}
	written := osconfig.Paths(files)
	fmt.Fprintf(out, "stage0 agent: wrote %s's network (%s, boot NIC %s), hostname and keys\n", sys.Hostname, sys.Network, mac)

	// An empty machine-id: systemd makes a new one on first boot.
	id := filepath.Join(root, "etc", "machine-id")
	if b, err := os.ReadFile(id); err == nil && len(b) > 0 {
		if err := os.Remove(id); err != nil {
			return nil, err
		}
		if err := os.WriteFile(id, nil, 0o444); err != nil {
			return nil, err
		}
		written = append(written, "/etc/machine-id")
	}
	// The image's host keys, replaced by the image's own ssh-keygen.
	keys, _ := filepath.Glob(filepath.Join(root, "etc", "ssh", "ssh_host_*_key*"))
	if len(keys) > 0 {
		for _, k := range keys {
			if err := os.Remove(k); err != nil {
				return nil, err
			}
		}
		// ssh-keygen needs /dev (/dev/null) in the root.
		dev := filepath.Join(root, "dev")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			return nil, err
		}
		if err := syscall.Mount("/dev", dev, "", syscall.MS_BIND, ""); err != nil {
			return nil, fmt.Errorf("binding /dev into the installed root: %w", err)
		}
		cmd := exec.Command("/usr/bin/ssh-keygen", "-A")
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
		cmd.Dir = "/"
		b, err := cmd.CombinedOutput()
		if uerr := syscall.Unmount(dev, 0); uerr != nil && err == nil {
			return nil, fmt.Errorf("unmounting %s: %w", dev, uerr)
		}
		if err != nil {
			return nil, fmt.Errorf("new SSH host keys (ssh-keygen -A in the installed root): %v: %s", err, b)
		}
		written = append(written, "/etc/ssh/ssh_host_*")
	}
	return written, nil
}

var findBootMAC = bootMAC // tests replace it

// bootMAC is the MAC of the interface that reaches server (a URL): the
// one the agent booted on.
func bootMAC(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	// UDP: no packet is sent, the kernel only picks the route.
	c, err := net.Dial("udp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", err
	}
	local := c.LocalAddr().(*net.UDPAddr).IP
	c.Close()
	ifs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) && len(ifc.HardwareAddr) == 6 {
				return ifc.HardwareAddr.String(), nil
			}
		}
	}
	return "", errors.New("no Ethernet interface has " + local.String())
}
