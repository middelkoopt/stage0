package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/dhcp6d"
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/httpd"
	"github.com/middelkoopt/stage0/internal/neigh"
	"github.com/middelkoopt/stage0/internal/radv"
	"github.com/middelkoopt/stage0/internal/tftpd"
)

// leaseTime is how long DHCP leases last; nodes renew well inside it.
const leaseTime = time.Hour

func (a *App) serveCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "serve <network> --root DIR",
		Short: "Answer DHCPv4, TFTP and HTTP for network-booting nodes on one network",
		Long: "Serve DHCPv4 (PXE clients, then the same nodes' kernels) and read-only TFTP\n" +
			"and HTTP (port 80) from --root on one network from site.yaml, until\n" +
			"interrupted or --timeout if given. Nodes stage0 installed on the network\n" +
			"(the install records) keep their address and boot from their disks; with no\n" +
			"--root, only DHCP runs. Everything stays on the network's interface, which\n" +
			"must already carry the network's address. Each request is logged (JSON\n" +
			"lines with -o json).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			site, err := config.LoadSite(a.sitePath)
			if err != nil {
				return err
			}
			n, err := site.Network(args[0])
			if err != nil {
				return err
			}
			var h slog.Handler = slog.NewTextHandler(a.Stdout, nil)
			if a.output == "json" {
				h = slog.NewJSONHandler(a.Stdout, nil)
			}
			log := slog.New(h).With("network", n.Name)

			// Runs until interrupted unless --timeout is given.
			ctx, cancel := context.WithCancel(cmd.Context())
			if cmd.Flags().Changed("timeout") {
				ctx, cancel = a.context(cmd)
			}
			defer cancel()
			installed, err := a.installedOn(n, "")
			if err != nil {
				return err
			}
			// Agents booted by hand report here too; serve just logs them.
			report := func(from, kind string, body []byte) {
				log.Info("agent: "+kind, "from", from, "body", string(body))
			}
			return a.serve(ctx, n, root, a.newLeases(n, installed, log), report, log)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "directory of boot files to serve over TFTP and HTTP (none: DHCP only)")
	return cmd
}

// installedLeaseTime is installed nodes' lease, long so they keep their
// address through a stage0 restart.
const installedLeaseTime = 24 * time.Hour

func (a *App) newLeases(n config.Network, installed map[string]netip.Addr, log *slog.Logger) *dhcpd.Server {
	return dhcpd.New(dhcpd.Config{
		Server: n.Server, Subnet: n.Subnet,
		PoolFirst: n.PoolFirst, PoolLast: n.PoolLast, LeaseTime: leaseTime,
		Installed: installed, InstalledLeaseTime: installedLeaseTime,
	}, log)
}

// serve answers DHCPv4 with leases, and TFTP and HTTP from root (if
// any), on network n until ctx is done. report, if set, receives what
// agents POST.
func (a *App) serve(ctx context.Context, n config.Network, root string, leases *dhcpd.Server,
	report func(from, kind string, body []byte), log *slog.Logger) error {
	var files *tftpd.Server
	var web *httpd.Server
	if root != "" {
		var err error
		if files, err = tftpd.New(root, log); err != nil {
			return err
		}
		if web, err = httpd.New(root, log); err != nil {
			return err
		}
		web.Report = report
	}
	socks, err := a.OpenNetwork(ctx, n)
	if err != nil {
		return err
	}
	log.Info("serving", "interface", n.Interface, "address", n.Server, "pool", n.Pool, "root", root,
		"installed", len(leases.InstalledNodes()))
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return leases.Serve(ctx, socks.DHCP) })
	if n.IPv6Prefix.IsValid() {
		g.Go(func() error {
			// Without CAP_NET_RAW, the rest still runs.
			if err := radv.New(n.IPv6Prefix, log).Serve(ctx, n.Interface); err != nil {
				log.Warn("router advertisements off (run the copy with CAP_NET_RAW: /usr/local/bin/stage0)", "err", err)
			}
			return nil
		})
	}
	if socks.DHCP6 != nil {
		ifc, _ := net.InterfaceByName(n.Interface)
		var mac net.HardwareAddr
		if ifc != nil {
			mac = ifc.HardwareAddr
		}
		// The Bootfile-URL names the global address: with the link-local
		// one the Pi took the reply but didn't go on to TFTP (docs/netboot.md).
		leases6 := dhcp6d.New(dhcp6d.Config{Server: n.Server6, Prefix: n.IPv6Prefix, MAC: mac}, log)
		// A Pi's bootloader doesn't answer neighbour solicitations for its
		// DHCPv6 address, so the host is told where it is.
		leases6.IsInstalled = leases.IsInstalled
		leases6.OnLease = func(ip netip.Addr, mac net.HardwareAddr) {
			if err := neigh.Add(n.Interface, ip, mac); err != nil {
				log.Warn("dhcpv6: neighbour entry", "ip", ip, "err", err)
			}
		}
		log.Info("serving IPv6 netboot (experimental)", "address", n.Server6)
		g.Go(func() error { return leases6.Serve(ctx, socks.DHCP6) })
		if root != "" {
			g.Go(func() error { return files.Serve(ctx, socks.TFTP6) })
			g.Go(func() error { return web.Serve(ctx, socks.HTTP6) })
		} else {
			socks.TFTP6.Close()
			socks.HTTP6.Close()
		}
	}
	if root != "" {
		g.Go(func() error { return files.Serve(ctx, socks.TFTP) })
		g.Go(func() error { return web.Serve(ctx, socks.HTTP) })
	} else {
		socks.TFTP.Close()
		socks.HTTP.Close()
	}
	err = g.Wait()
	log.Info("stopped")
	return err
}

// Sockets are serve's listening sockets. DHCP6 and TFTP6 are nil when
// the network has no IPv6 prefix, or the interface lacks <prefix>::1.
type Sockets struct {
	DHCP, TFTP   net.PacketConn
	HTTP         net.Listener
	DHCP6, TFTP6 net.PacketConn
	HTTP6        net.Listener
}

// openNetwork checks the network's interface and opens serve's sockets
// pinned to it.
func openNetwork(ctx context.Context, n config.Network) (Sockets, error) {
	var s Sockets
	if err := checkInterface(n.Interface, n.Server); err != nil {
		return s, fmt.Errorf("network %q: %w", n.Name, err)
	}
	var err error
	if s.DHCP, err = dhcpd.Listen(ctx, n.Interface); err != nil {
		return s, err
	}
	if s.TFTP, err = tftpd.Listen(ctx, n.Interface, n.Server); err != nil {
		s.DHCP.Close()
		return s, err
	}
	if s.HTTP, err = httpd.Listen(ctx, n.Interface, n.Server); err != nil {
		s.DHCP.Close()
		s.TFTP.Close()
		return s, err
	}
	// IPv6 netboot, if the interface carries <prefix>::1.
	if n.Server6.IsValid() && checkInterfaceAddr(n.Interface, n.Server6) {
		if s.DHCP6, err = dhcp6d.Listen(ctx, n.Interface); err == nil {
			// [::], pinned to the interface: the library can't listen on an
			// address with a zone (fe80::…%eth0.3).
			if s.TFTP6, err = tftpd.Listen(ctx, n.Interface, netip.IPv6Unspecified()); err != nil {
				s.DHCP6.Close()
				s.DHCP6 = nil
			} else if s.HTTP6, err = httpd.Listen(ctx, n.Interface, n.Server6); err != nil {
				s.DHCP6.Close()
				s.TFTP6.Close()
				s.DHCP6, s.TFTP6 = nil, nil
			}
		}
		err = nil // IPv6 netboot is optional; serve says what runs
	}
	return s, nil
}

// checkInterfaceAddr reports whether ifname carries addr.
func checkInterfaceAddr(ifname string, addr netip.Addr) bool {
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		return false
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr() == addr {
			return true
		}
	}
	return false
}

// checkInterface makes sure ifname is up and is the only interface carrying
// addr, so answers can only go where the site config says.
func checkInterface(ifname string, addr netip.Addr) error {
	if _, err := net.InterfaceByName(ifname); err != nil {
		return fmt.Errorf("no interface %s on this host", ifname)
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			return err
		}
		has := false
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr() == addr {
				has = true
			}
		}
		switch {
		case ifc.Name == ifname:
			if ifc.Flags&net.FlagUp == 0 {
				return fmt.Errorf("interface %s is down", ifname)
			}
			if !has {
				return fmt.Errorf("interface %s does not carry %s; add it to the host's network config first", ifname, addr)
			}
		case has:
			return fmt.Errorf("%s is also on interface %s, not only %s", addr, ifc.Name, ifname)
		}
	}
	return nil
}
