package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/tftpd"
)

// leaseTime is how long DHCP leases last; nodes renew well inside it.
const leaseTime = time.Hour

func (a *App) serveCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "serve <network> --root DIR",
		Short: "Answer DHCPv4 and TFTP for network-booting nodes on one network",
		Long: "Serve DHCPv4 (PXE clients only, such as a Pi's bootloader) and read-only TFTP\n" +
			"from --root on one network from site.yaml, until interrupted or --timeout if\n" +
			"given. Everything stays on the network's interface, which must already carry\n" +
			"the network's address. Each request is logged (JSON lines with -o json).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			if root == "" {
				return errors.New("--root is required: the directory of boot files to serve")
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

			files, err := tftpd.New(root, log)
			if err != nil {
				return err
			}
			leases := dhcpd.New(dhcpd.Config{
				Server: n.Server, Subnet: n.Subnet,
				PoolFirst: n.PoolFirst, PoolLast: n.PoolLast, LeaseTime: leaseTime,
			}, log)

			// Runs until interrupted unless --timeout is given.
			ctx, cancel := context.WithCancel(cmd.Context())
			if cmd.Flags().Changed("timeout") {
				ctx, cancel = a.context(cmd)
			}
			defer cancel()

			dconn, tconn, err := a.OpenNetwork(ctx, n)
			if err != nil {
				return err
			}
			log.Info("serving", "interface", n.Interface, "address", n.Server, "pool", n.Pool, "root", root)
			g, ctx := errgroup.WithContext(ctx)
			g.Go(func() error { return leases.Serve(ctx, dconn) })
			g.Go(func() error { return files.Serve(ctx, tconn) })
			err = g.Wait()
			log.Info("stopped")
			return err
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "directory of boot files to serve over TFTP")
	return cmd
}

// openNetwork checks the network's interface and opens the DHCP and TFTP
// sockets pinned to it.
func openNetwork(ctx context.Context, n config.Network) (dhcp, tftp net.PacketConn, err error) {
	if err := checkInterface(n.Interface, n.Server); err != nil {
		return nil, nil, fmt.Errorf("network %q: %w", n.Name, err)
	}
	if dhcp, err = dhcpd.Listen(ctx, n.Interface); err != nil {
		return nil, nil, err
	}
	if tftp, err = tftpd.Listen(ctx, n.Interface, n.Server); err != nil {
		dhcp.Close()
		return nil, nil, err
	}
	return dhcp, tftp, nil
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
