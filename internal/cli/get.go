package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/netswitch"
)

func (a *App) getCommand() *cobra.Command {
	get := &cobra.Command{
		Use:   "get",
		Short: "Show the current state of something",
	}
	var switchName string
	get.PersistentFlags().StringVar(&switchName, "switch", "", "switch to read (default: the only one in the site config)")

	status := func(cmd *cobra.Command) (netswitch.Status, error) {
		if err := a.checkOutput(); err != nil {
			return netswitch.Status{}, err
		}
		r, err := a.switchReader(switchName)
		if err != nil {
			return netswitch.Status{}, err
		}
		ctx, cancel := a.context(cmd)
		defer cancel()
		return r.Status(ctx)
	}

	get.AddCommand(&cobra.Command{
		Use:   "ports",
		Short: "Switch ports: label, link, PoE and VLANs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := status(cmd)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.writeJSON(st.Ports)
			}
			return a.writePorts(st)
		},
	})
	get.AddCommand(&cobra.Command{
		Use:   "vlans",
		Short: "VLANs defined on the switch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := status(cmd)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.writeJSON(st.VLANs)
			}
			w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME")
			for _, v := range st.VLANs {
				fmt.Fprintf(w, "%d\t%s\n", v.ID, v.Name)
			}
			return w.Flush()
		},
	})
	return get
}

func (a *App) writePorts(st netswitch.Status) error {
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PORT\tNAME\tLINK\tSPEED\tPOE\tPVID\tTAGGED")
	for _, p := range st.Ports {
		link, speed, poe := "down", dash(p.Speed), "-"
		if p.Linked {
			link = "up"
		}
		if p.PoE != nil {
			poe = map[bool]string{true: "on", false: "off"}[*p.PoE]
		}
		tagged := make([]string, len(p.Tagged))
		for i, id := range p.Tagged {
			tagged[i] = vlanLabel(st, id)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.ID, dash(p.Name), link, speed, poe, vlanLabel(st, p.PVID), dash(strings.Join(tagged, ",")))
	}
	return w.Flush()
}

// vlanLabel shows a VLAN as name(id), or the bare id if it has no name.
func vlanLabel(st netswitch.Status, id int) string {
	if v, ok := st.VLAN(id); ok && v.Name != "" {
		return fmt.Sprintf("%s(%d)", v.Name, id)
	}
	return strconv.Itoa(id)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *App) writeJSON(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
