// Package cli is stage0's command line: verb first, then the thing acted on
// ("stage0 get ports"). Every command can print JSON (-o json) so a person and
// an agent can drive the same steps.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/netswitch"
	"github.com/middelkoopt/stage0/internal/netswitch/gs316ep"
)

// App holds what commands need from the outside world, so tests can replace it.
type App struct {
	Stdout     io.Writer
	OpenSwitch func(config.Switch, config.SwitchSecret) (netswitch.Reader, error)

	sitePath    string
	secretsPath string
	output      string
	timeout     time.Duration
}

// New returns the stage0 root command wired to the real world.
func New() *cobra.Command {
	return (&App{Stdout: os.Stdout, OpenSwitch: openSwitch}).Command()
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

	root.AddCommand(a.getCommand())
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

// switchReader loads the site and secrets and opens the named switch.
func (a *App) switchReader(name string) (netswitch.Reader, error) {
	site, err := config.LoadSite(a.sitePath)
	if err != nil {
		return nil, err
	}
	sw, err := site.Switch(name)
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
func openSwitch(sw config.Switch, sec config.SwitchSecret) (netswitch.Reader, error) {
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
