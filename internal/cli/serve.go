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
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/httpd"
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
			return a.serve(ctx, n, root, a.newLeases(n, installed, log), nil, log)
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

// Sockets are serve's listening sockets.
type Sockets struct {
	DHCP, TFTP net.PacketConn
	HTTP       net.Listener
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
	return s, nil
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
