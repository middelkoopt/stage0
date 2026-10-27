package cli

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/config"
)

// nodeInfo is a node as `get nodes` shows it: the site's settings and,
// once installed, what the install recorded.
type nodeInfo struct {
	Name      string         `json:"name"`
	Hostname  string         `json:"hostname"`
	Network   string         `json:"network,omitempty"`
	Disk      string         `json:"disk,omitempty"`
	VM        bool           `json:"vm,omitempty"`
	Console   string         `json:"console,omitempty"` // serial device
	BMC       string         `json:"bmc,omitempty"`     // its virtual BMC, host:port
	Installed *installedInfo `json:"installed"`         // null until installed
}

type installedInfo struct {
	Image string `json:"image"`
	Time  string `json:"time"` // when the record was written, RFC 3339
	MAC   string `json:"mac,omitempty"`
	IPv4  string `json:"ipv4,omitempty"` // its lease from stage0
	IPv6  string `json:"ipv6,omitempty"` // found after boot (often link-local)
	Port  int    `json:"port,omitempty"` // switch port
}

func (a *App) nodesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "nodes",
		Short: "Nodes: site settings and what was installed (for stage 1: -o json)",
		Long: "List the nodes in site.yaml with their network, disk, console and virtual\n" +
			"BMC, and for those stage0 installed, the image, when, MAC and addresses\n" +
			"from the install records. Nothing is looked up on the network.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			site, err := config.LoadSite(a.sitePath)
			if err != nil {
				return err
			}
			dir, err := a.stateDir()
			if err != nil {
				return err
			}
			var nodes []nodeInfo
			for _, n := range site.Nodes {
				ni := nodeInfo{Name: n.Name, Hostname: n.Hostname, Network: n.Network, Disk: n.Disk, VM: n.VM != nil}
				if n.Console != nil {
					ni.Console = n.Console.Device
				}
				if n.BMC != nil {
					host := "?"
					if nw, err := oneNetwork(site, n.Network); err == nil {
						host = nw.Server.String()
					}
					ni.BMC = fmt.Sprintf("%s:%d", host, n.BMC.Port)
				}
				if r, err := a.loadRecord(n.Name); err == nil {
					in := &installedInfo{Image: r.Image, MAC: r.MAC, IPv4: r.IPv4, IPv6: r.Address, Port: r.Port}
					if fi, err := os.Stat(filepath.Join(dir, "nodes", n.Name+".json")); err == nil {
						in.Time = fi.ModTime().Format(time.RFC3339)
					}
					ni.Network = cmp.Or(r.Network, ni.Network)
					ni.Installed = in
				}
				nodes = append(nodes, ni)
			}
			if a.output == "json" {
				return a.writeJSON(nodes)
			}
			w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NODE\tNETWORK\tBMC\tIMAGE\tINSTALLED\tIPV4\tIPV6")
			for _, n := range nodes {
				img, when, v4, v6 := "-", "-", "-", "-"
				if in := n.Installed; in != nil {
					img, when, v4, v6 = in.Image, cmp.Or(in.Time, "-"), cmp.Or(in.IPv4, "-"), cmp.Or(in.IPv6, "-")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, cmp.Or(n.Network, "-"), cmp.Or(n.BMC, "-"), img, when, v4, v6)
			}
			return w.Flush()
		},
	}
}
