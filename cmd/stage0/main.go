// Command stage0 installs a head node from nothing but this repo and a
// connection to the provisioning network.
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
	if err := cli.New().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "stage0:", err)
		os.Exit(1)
	}
}
