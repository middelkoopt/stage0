// Command stage0 installs a head node from nothing but this repo and a
// connection to the provisioning network.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/cli"
)

func main() {
	// As /init of stage0's initramfs, stage0 is the agent on the node.
	if os.Getpid() == 1 {
		agent.Main()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.New().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "stage0:", err)
		os.Exit(1)
	}
}
