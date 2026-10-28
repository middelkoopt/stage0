// Package cli is stage0's command line: verb first, then the thing acted on
// ("stage0 get ports"). Every command can print JSON (-o json) so a person and
// an agent can drive the same steps.
package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/console"
	"github.com/middelkoopt/stage0/internal/initrd"
	"github.com/middelkoopt/stage0/internal/neigh"
	"github.com/middelkoopt/stage0/internal/netswitch"
	"github.com/middelkoopt/stage0/internal/netswitch/gs316ep"
	"github.com/middelkoopt/stage0/internal/reach"
	"github.com/middelkoopt/stage0/internal/vswitch"
)

// App holds what commands need from the outside world, so tests can replace it.
type App struct {
	Stdout      io.Writer
	OpenSwitch  func(config.Switch, config.SwitchSecret) (netswitch.Controller, error)
	OpenConsole func(device string, baud int) (io.ReadWriteCloser, error)
	OpenNetwork func(context.Context, config.Network) (Sockets, error)
	CheckStatic func(path string) error
	Executable  func() (string, error) // this binary, packed as the agent
	FindAddress func(ctx context.Context, ifname string, macs []net.HardwareAddr) ([]netip.Addr, error)
	Reach       func(ctx context.Context, addr string) reach.Result // waits until addr answers ping and SSH
	OpenTap     func(name string) (vswitch.Port, error)
	StartVM     func(args []string, log io.Writer) (VMProcess, error) // runs QEMU

	sitePath    string
	secretsPath string
	output      string
	timeout     time.Duration
	switchName  string
	statePath   string
	dryRun      bool
}

// New returns the stage0 root command wired to the real world.
func New() *cobra.Command {
	return (&App{Stdout: os.Stdout, OpenSwitch: openSwitch, OpenConsole: console.Open, OpenNetwork: openNetwork, CheckStatic: initrd.CheckStatic, Executable: os.Executable, FindAddress: neigh.Find, Reach: reach.Wait, OpenTap: vswitch.Tap, StartVM: startQEMU}).Command()
}

// Command builds the command tree.
func (a *App) Command() *cobra.Command {
	root := &cobra.Command{
		Use:           "stage0",
		Short:         "Install a head node from nothing but this repo and the provisioning network",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	f := root.PersistentFlags()
	f.StringVar(&a.sitePath, "site", config.DefaultSitePath, "site configuration file")
	f.StringVar(&a.secretsPath, "secrets", config.DefaultSecretsPath, "credentials file (mode 600, git-ignored)")
	f.StringVarP(&a.output, "output", "o", "text", "output format: text or json")
	f.DurationVar(&a.timeout, "timeout", 30*time.Second, "time limit for the whole command")
	f.StringVar(&a.switchName, "switch", "", "switch to use (default: the only one in the site config)")
	f.StringVar(&a.statePath, "state", "", "records of installed nodes (default: $XDG_STATE_HOME/stage0 or ~/.local/state/stage0)")

	root.AddCommand(a.getCommand(), a.powerCommand(), a.setCommand(), a.consoleCommand(), a.serveCommand(), a.buildCommand(), a.installCommand(), a.checkCommand(), a.vswitchCommand(), a.bmcCommand(), a.versionCommand(), agentCommand())
	return root
}

// context applies --timeout to the command's context.
func (a *App) context(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), a.timeout)
}

// checkOutput validates -o before any work is done.
func (a *App) checkOutput() error {
	switch a.output {
	case "text", "json":
		return nil
	}
	return fmt.Errorf("unknown output format %q (want text or json)", a.output)
}

// openSite loads the site and secrets and opens the switch named by --switch.
func (a *App) openSite() (netswitch.Controller, error) {
	site, err := config.LoadSite(a.sitePath)
	if err != nil {
		return nil, err
	}
	sw, err := site.Switch(a.switchName)
	if err != nil {
		return nil, err
	}
	secrets, err := config.LoadSecrets(a.secretsPath)
	if err != nil {
		return nil, err
	}
	return a.OpenSwitch(sw, secrets.Switches[sw.Name])
}

// openSwitch is the real driver lookup.
func openSwitch(sw config.Switch, sec config.SwitchSecret) (netswitch.Controller, error) {
	switch sw.Driver {
	case "gs316ep":
		c, err := gs316ep.New(sw.Address, gs316ep.Credentials{Password: sec.Password, LoginHash: sec.LoginHash}, nil)
		if err != nil {
			return nil, fmt.Errorf("switch %q: %w", sw.Name, err)
		}
		return c, nil
	}
	return nil, fmt.Errorf("switch %q: no driver %q", sw.Name, sw.Driver)
}

// agentCommand runs the agent from a container installer's dracut hook
// (docs/containers.md). Hidden: only the hook stage0 appends calls it.
func agentCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "agent",
		Short:  "Run as the install agent inside an installer's initramfs",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run:    func(*cobra.Command, []string) { agent.Hook() },
	}
}
