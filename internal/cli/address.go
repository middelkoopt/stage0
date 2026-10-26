package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/config"
)

// addressWait is how long install looks for the node's address at the end.
const addressWait = 20 * time.Second

// stateDir is where stage0 keeps records of what it installed:
// --state, else $XDG_STATE_HOME/stage0, else ~/.local/state/stage0.
func (a *App) stateDir() (string, error) {
	if a.statePath != "" {
		return a.statePath, nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "stage0"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "stage0"), nil
}

// saveRecord keeps install's result for a node, for get address later.
func (a *App) saveRecord(r installResult) error {
	dir, err := a.stateDir()
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "nodes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, r.Node+".json"), append(b, '\n'), 0o644)
}

func (a *App) loadRecord(node string) (installResult, error) {
	var r installResult
	dir, err := a.stateDir()
	if err != nil {
		return r, err
	}
	p := filepath.Join(dir, "nodes", node+".json")
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("no record of installing %s (%s); stage0 install writes it", node, p)
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// installedOn returns the nodes installed on network n, from the install
// records (MAC -> address), leaving out the node named except.
func (a *App) installedOn(n config.Network, except string) (map[string]netip.Addr, error) {
	dir, err := a.stateDir()
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "nodes", "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]netip.Addr{}
	for _, f := range files {
		var r installResult
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		ip, err := netip.ParseAddr(r.IPv4)
		if r.Node == except || r.Network != n.Name || r.MAC == "" || err != nil {
			continue
		}
		if !n.Subnet.Contains(ip) {
			return nil, fmt.Errorf("%s: %s is not on network %s (%s)", f, ip, n.Name, n.Subnet)
		}
		hw, err := net.ParseMAC(r.MAC)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out[hw.String()] = ip
	}
	return out, nil
}

// macs are the node's MAC addresses, from the agent's report.
func (r installResult) macs() ([]net.HardwareAddr, error) {
	var rep agent.Report
	if len(r.Report) > 0 {
		if err := json.Unmarshal(r.Report, &rep); err != nil {
			return nil, fmt.Errorf("%s's install record: %w", r.Node, err)
		}
	}
	var out []net.HardwareAddr
	for _, i := range rep.Interfaces {
		if hw, err := net.ParseMAC(i.MAC); err == nil {
			out = append(out, hw)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s's install record has no MAC address", r.Node)
	}
	return out, nil
}

// address finds an installed node's IPv6 address on network n, as
// "fe80::…%iface".
func (a *App) address(ctx context.Context, r installResult, n config.Network) (string, error) {
	macs, err := r.macs()
	if err != nil {
		return "", err
	}
	addrs, err := a.FindAddress(ctx, n.Interface, macs)
	if err != nil {
		return "", err
	}
	ad := addrs[0]
	if ad.IsLinkLocalUnicast() {
		return ad.String() + "%" + n.Interface, nil
	}
	return ad.String(), nil
}

func (a *App) addressCommand() *cobra.Command {
	var network string
	cmd := &cobra.Command{
		Use:   "address <node>",
		Short: "An installed node's IPv6 address, found on its network",
		Long: "Find a node stage0 installed by its MAC address (from the install record)\n" +
			"on the network's interface, and print its IPv6 address, link-local first,\n" +
			"with an ssh command to reach it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			site, err := config.LoadSite(a.sitePath)
			if err != nil {
				return err
			}
			n, err := oneNetwork(site, network)
			if err != nil {
				return err
			}
			r, err := a.loadRecord(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := a.context(cmd)
			defer cancel()
			addr, err := a.address(ctx, r, n)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.writeJSON(map[string]string{"node": r.Node, "address": addr})
			}
			fmt.Fprintf(a.Stdout, "%s: %s\n  ssh root@%s\n", r.Node, addr, addr)
			return nil
		},
	}
	cmd.Flags().StringVar(&network, "network", "", "network to look on (default: the only one in site.yaml)")
	return cmd
}
