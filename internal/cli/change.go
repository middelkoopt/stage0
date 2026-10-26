package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/netswitch"
)

// Write commands name a node by its switch-port label (or port number), show
// the change before -> after, and support --dry-run. The switch driver
// checks every write by reading the switch back.

func (a *App) powerCommand() *cobra.Command {
	power := &cobra.Command{
		Use:   "power",
		Short: "Power a node on, off, or cycle it (PoE on its switch port)",
	}
	power.PersistentFlags().BoolVar(&a.dryRun, "dry-run", false, "show the change without making it")

	for _, on := range []bool{true, false} {
		verb := map[bool]string{true: "on", false: "off"}[on]
		power.AddCommand(&cobra.Command{
			Use:   verb + " <node>",
			Short: "Turn PoE " + verb + " on the node's switch port",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.changePort(cmd, args[0], portOp{
					action: "poe-" + verb,
					plan: func(st netswitch.Status, p netswitch.Port) (netswitch.Port, error) {
						return st.PlanPoE(p.ID, on)
					},
					apply: func(ctx context.Context, sw netswitch.Controller, want netswitch.Port) (netswitch.Change, error) {
						return sw.SetPoE(ctx, want.ID, on)
					},
					show: showPoE,
				})
			},
		})
	}
	power.AddCommand(&cobra.Command{
		Use:   "cycle <node>",
		Short: "Turn PoE off and on again on the node's switch port",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.changePort(cmd, args[0], portOp{
				action: "poe-cycle",
				plan: func(_ netswitch.Status, p netswitch.Port) (netswitch.Port, error) {
					switch {
					case p.PoE == nil:
						return p, fmt.Errorf("%s has no PoE", label(p))
					case !*p.PoE:
						return p, fmt.Errorf("%s: PoE is off, so there is nothing to cycle; turn it on instead", label(p))
					}
					return p, nil
				},
				apply: func(ctx context.Context, sw netswitch.Controller, want netswitch.Port) (netswitch.Change, error) {
					return sw.CyclePoE(ctx, want.ID)
				},
				show: showPoE,
			})
		},
	})
	return power
}

func (a *App) setCommand() *cobra.Command {
	set := &cobra.Command{
		Use:   "set",
		Short: "Change the configuration of something",
	}
	set.PersistentFlags().BoolVar(&a.dryRun, "dry-run", false, "show the change without making it")

	var pvid string
	var tagged []string
	port := &cobra.Command{
		Use:   "port <node>",
		Short: "Set a switch port's untagged VLAN (PVID) and tagged VLANs",
		Long: "Set a switch port's untagged VLAN (--pvid) and tagged VLANs (--tagged),\n" +
			"by name or ID. What isn't given stays as it is; --tagged= clears the tagged VLANs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			setPVID, setTagged := cmd.Flags().Changed("pvid"), cmd.Flags().Changed("tagged")
			if !setPVID && !setTagged {
				return fmt.Errorf("nothing to change: give --pvid, --tagged, or both")
			}
			return a.changePort(cmd, args[0], portOp{
				action: "set-vlans",
				plan: func(st netswitch.Status, p netswitch.Port) (netswitch.Port, error) {
					id, ids := p.PVID, p.Tagged
					if setPVID {
						v, err := st.FindVLAN(pvid)
						if err != nil {
							return p, err
						}
						id = v.ID
					}
					if setTagged {
						ids = []int{}
						for _, ref := range tagged {
							v, err := st.FindVLAN(ref)
							if err != nil {
								return p, err
							}
							ids = append(ids, v.ID)
						}
					}
					return st.PlanVLANs(p.ID, id, ids)
				},
				apply: func(ctx context.Context, sw netswitch.Controller, want netswitch.Port) (netswitch.Change, error) {
					return sw.SetVLANs(ctx, want.ID, want.PVID, want.Tagged)
				},
				show: showVLANs,
			})
		},
	}
	port.Flags().StringVar(&pvid, "pvid", "", "untagged VLAN, by name or ID")
	port.Flags().StringSliceVar(&tagged, "tagged", nil, "tagged VLANs, by name or ID, comma separated")
	set.AddCommand(port)
	return set
}

// portOp is one change to one switch port.
type portOp struct {
	action string // reported in JSON: poe-on, poe-off, poe-cycle, set-vlans
	// plan returns the port as it should end up, or an error if the change
	// isn't possible; it runs before anything is written.
	plan func(netswitch.Status, netswitch.Port) (netswitch.Port, error)
	// apply makes the change; the driver re-reads the switch, so it acts on
	// the switch's state at that moment, not on the state plan saw.
	apply func(ctx context.Context, sw netswitch.Controller, want netswitch.Port) (netswitch.Change, error)
	show  func(netswitch.Status, netswitch.Port) string // the settings this op touches
}

// changeResult is what a write command reports.
type changeResult struct {
	Node    string         `json:"node"`
	Port    int            `json:"port"`
	Action  string         `json:"action"`
	DryRun  bool           `json:"dryRun"`
	Changed bool           `json:"changed"`
	Before  netswitch.Port `json:"before"`
	After   netswitch.Port `json:"after"`
}

// changePort finds the node's port, plans the change, and makes it unless
// --dry-run is set.
func (a *App) changePort(cmd *cobra.Command, node string, op portOp) error {
	if err := a.checkOutput(); err != nil {
		return err
	}
	sw, err := a.openSite()
	if err != nil {
		return err
	}
	ctx, cancel := a.context(cmd)
	defer cancel()

	st, err := sw.Status(ctx)
	if err != nil {
		return err
	}
	p, err := st.Find(node)
	if err != nil {
		return err
	}
	want, err := op.plan(st, p)
	if err != nil {
		return err
	}

	ch := netswitch.Change{Before: p, After: want}
	if !a.dryRun {
		if ch, err = op.apply(ctx, sw, want); err != nil {
			return err
		}
	}
	r := changeResult{
		Node: p.Name, Port: p.ID, Action: op.action, DryRun: a.dryRun,
		Changed: ch.Changed(), Before: ch.Before, After: ch.After,
	}
	if a.output == "json" {
		return a.writeJSON(r)
	}

	var line string
	switch {
	case op.action == "poe-cycle" && a.dryRun:
		line = "would cycle PoE on " + label(p)
	case op.action == "poe-cycle":
		line = label(p) + ": PoE cycled"
	case !r.Changed:
		line = label(p) + ": already " + op.show(st, ch.After)
	case a.dryRun:
		line = "would change " + label(p) + ": " + op.show(st, ch.Before) + " -> " + op.show(st, ch.After)
	default:
		line = label(p) + ": " + op.show(st, ch.Before) + " -> " + op.show(st, ch.After)
	}
	_, err = fmt.Fprintln(a.Stdout, line)
	return err
}

// label names a port for people: "k53 (port 2)", or "port 3" if unlabeled.
func label(p netswitch.Port) string {
	if p.Name == "" {
		return fmt.Sprintf("port %d", p.ID)
	}
	return fmt.Sprintf("%s (port %d)", p.Name, p.ID)
}

func showPoE(_ netswitch.Status, p netswitch.Port) string {
	if p.PoE == nil {
		return "no PoE"
	}
	return map[bool]string{true: "PoE on", false: "PoE off"}[*p.PoE]
}

func showVLANs(st netswitch.Status, p netswitch.Port) string {
	tagged := make([]string, len(p.Tagged))
	for i, id := range p.Tagged {
		tagged[i] = vlanLabel(st, id)
	}
	return "PVID " + vlanLabel(st, p.PVID) + ", tagged " + dash(strings.Join(tagged, ","))
}
