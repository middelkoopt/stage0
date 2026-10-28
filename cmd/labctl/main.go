// Command labctl drives the lab stage0 installs in: switch ports and
// VLANs, PoE power, serial consoles, and BMCs for the Pis.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/middelkoopt/stage0/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.NewLab().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "labctl:", err)
		os.Exit(1)
	}
}
