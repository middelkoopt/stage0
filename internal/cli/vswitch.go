package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/vswitch"
)

func (a *App) vswitchCommand() *cobra.Command {
	var socket, tap string
	cmd := &cobra.Command{
		Use:   "vswitch --socket PATH",
		Short: "Run a virtual Ethernet switch for QEMU VMs, with the host's tap as a port",
		Long: "Run a learning Ethernet switch that QEMU VMs join over a Unix socket\n" +
			"(-netdev stream,addr.type=unix,addr.path=PATH), with the tap device --tap\n" +
			"(the host's side of the test network, e.g. stage0tap0 at 10.0.9.1) as one\n" +
			"more port, so stage0 serve on that network answers the VMs. Runs until\n" +
			"interrupted, or --timeout if given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if socket == "" {
				return errors.New("--socket is required")
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			if cmd.Flags().Changed("timeout") {
				ctx, cancel = a.context(cmd)
			}
			defer cancel()
			if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			ln, err := net.Listen("unix", socket)
			if err != nil {
				return err
			}
			defer os.Remove(socket)
			log := slog.New(slog.NewTextHandler(a.Stdout, nil))
			s := vswitch.New(log)
			if tap != "" {
				p, err := a.OpenTap(tap)
				if err != nil {
					ln.Close()
					return err
				}
				go s.Attach(p)
				log.Info("vswitch: tap attached", "tap", tap)
			}
			fmt.Fprintf(a.Stdout, "vswitch on %s; VMs join with -netdev stream,id=n0,server=off,addr.type=unix,addr.path=%s\n", socket, socket)
			return s.Serve(ctx, ln)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "Unix socket VMs connect to")
	cmd.Flags().StringVar(&tap, "tap", "stage0tap0", "tap device to attach (empty: none)")
	return cmd
}
