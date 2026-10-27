package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os/signal"
	"slices"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/ipmi"
	"github.com/middelkoopt/stage0/internal/netswitch"
)

func (a *App) bmcCommand() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "bmc [node...]",
		Short: "Answer IPMI for nodes: power by PoE, SOL by the serial console",
		Long: "Run a virtual BMC (IPMI 2.0 over the LAN, as ipmitool -I lanplus speaks)\n" +
			"for each node with bmc: in site.yaml, or the nodes named: chassis power\n" +
			"switches its PoE port, sol activate opens its serial console. Each listens\n" +
			"on its own UDP port, on stage0's address on the node's network unless\n" +
			"--listen gives another; the login is bmc: in .secrets. Runs until\n" +
			"interrupted. Example, against k52's (port 6231):\n" +
			"  ipmitool -I lanplus -H 10.0.3.1 -p 6231 -U <user> -P <password> chassis power status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if a.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, a.timeout)
				defer cancel()
			}
			return a.bmc(ctx, args, listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to listen on (default: stage0's address on each node's network)")
	return cmd
}

func (a *App) bmc(ctx context.Context, names []string, listen string) error {
	site, err := config.LoadSite(a.sitePath)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(a.secretsPath)
	if err != nil {
		return err
	}
	if secrets.BMC == nil || secrets.BMC.User == "" || secrets.BMC.Password == "" {
		return fmt.Errorf("%s has no bmc: user and password", a.secretsPath)
	}
	var nodes []config.Node
	for _, n := range site.Nodes {
		if n.BMC != nil && (len(names) == 0 || slices.Contains(names, n.Name)) {
			nodes = append(nodes, n)
		}
	}
	for _, name := range names {
		if !slices.ContainsFunc(nodes, func(n config.Node) bool { return n.Name == name }) {
			return fmt.Errorf("node %q has no bmc: in %s", name, a.sitePath)
		}
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no node has bmc: in %s", a.sitePath)
	}
	sw, err := a.openSite()
	if err != nil {
		return err
	}
	ps := &poeSwitch{sw: sw}
	log := slog.New(slog.NewTextHandler(a.Stdout, nil))

	var wg sync.WaitGroup
	errs := make(chan error, len(nodes))
	for _, n := range nodes {
		addr := listen
		if addr == "" {
			nw, err := oneNetwork(site, n.Network)
			if err != nil {
				return fmt.Errorf("%s's bmc: %w", n.Name, err)
			}
			addr = nw.Server.String()
		}
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("--listen %q: %w", addr, err)
		}
		ap := netip.AddrPortFrom(ip, uint16(n.BMC.Port))
		conn, err := net.ListenPacket("udp", ap.String())
		if err != nil {
			return fmt.Errorf("%s's bmc: %w", n.Name, err)
		}
		log.Info("bmc: serving", "node", n.Name, "address", ap.String())
		s := &ipmi.Server{Name: n.Name, User: secrets.BMC.User, Password: secrets.BMC.Password, Log: log,
			Node: &bmcNode{a: a, node: n, sw: ps}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Serve(ctx, conn); err != nil {
				errs <- fmt.Errorf("%s's bmc: %w", n.Name, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	return errors.Join(slices.Collect(func(yield func(error) bool) {
		for e := range errs {
			if !yield(e) {
				return
			}
		}
	})...)
}

// poeSwitch serialises the BMCs' use of the switch (its web UI allows few
// sessions).
type poeSwitch struct {
	mu sync.Mutex
	sw netswitch.Controller
}

func (p *poeSwitch) port(ctx context.Context, name string) (netswitch.Port, error) {
	st, err := p.sw.Status(ctx)
	if err != nil {
		return netswitch.Port{}, err
	}
	port, err := st.Find(name)
	if err != nil {
		return port, err
	}
	if port.PoE == nil {
		return port, fmt.Errorf("%s has no PoE", label(port))
	}
	return port, nil
}

// bmcNode is a node as its virtual BMC sees it.
type bmcNode struct {
	a    *App
	node config.Node
	sw   *poeSwitch
}

func (b *bmcNode) PowerOn(ctx context.Context) (bool, error) {
	b.sw.mu.Lock()
	defer b.sw.mu.Unlock()
	port, err := b.sw.port(ctx, b.node.Name)
	if err != nil {
		return false, err
	}
	return *port.PoE, nil
}

func (b *bmcNode) SetPower(ctx context.Context, on bool) error {
	b.sw.mu.Lock()
	defer b.sw.mu.Unlock()
	port, err := b.sw.port(ctx, b.node.Name)
	if err != nil {
		return err
	}
	_, err = b.sw.sw.SetPoE(ctx, port.ID, on)
	return err
}

func (b *bmcNode) Console() (ipmi.Console, error) {
	if b.node.Console == nil {
		return nil, fmt.Errorf("node %q has no console", b.node.Name)
	}
	return b.a.OpenConsole(b.node.Console.Device, b.node.Console.Baud)
}
