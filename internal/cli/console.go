package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/console"
	"github.com/middelkoopt/stage0/internal/config"
)

func (a *App) consoleCommand() *cobra.Command {
	var until, logPath, send string
	cmd := &cobra.Command{
		Use:   "console <node>",
		Short: "Show a node's serial console, logging everything it prints",
		Long: "Show a node's serial console (its device is in site.yaml) and append it to\n" +
			"logs/<node>.log. Runs until interrupted, until --timeout if given, or until\n" +
			"the output matches --until. With -o json, prints only a summary at the end.\n" +
			"--send types text first (Go escapes: \\r for Enter, \\x03 for Ctrl-C), e.g.\n" +
			"  labctl console k52 --send 'root\\r' --until 'Password:'",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			text, err := strconv.Unquote(`"` + strings.ReplaceAll(send, `"`, `\"`) + `"`)
			if err != nil {
				return fmt.Errorf("--send %q: not valid escapes (use \\r, \\n, \\x03): %w", send, err)
			}
			var pattern *regexp.Regexp
			if until != "" {
				var err error
				if pattern, err = regexp.Compile(until); err != nil {
					return fmt.Errorf("--until: %w", err)
				}
			}
			site, err := config.LoadSite(a.sitePath)
			if err != nil {
				return err
			}
			node, err := site.Node(args[0])
			if err != nil {
				return err
			}
			if node.Console == nil {
				return fmt.Errorf("node %q has no console in %s", node.Name, a.sitePath)
			}
			if logPath == "" {
				logPath = filepath.Join("logs", node.Name+".log")
			}

			if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
				return err
			}
			log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer log.Close()

			port, err := a.OpenConsole(node.Console.Device, node.Console.Baud)
			if err != nil {
				return err
			}
			defer port.Close()
			fmt.Fprintf(log, "\n=== labctl console %s %s %s ===\n", node.Name, node.Console.Device, time.Now().Format(time.RFC3339))
			if text != "" {
				fmt.Fprintf(log, "=== labctl console %s sent %q ===\n", node.Name, text)
				if _, err := io.WriteString(port, text); err != nil {
					return fmt.Errorf("console %s: sending: %w", node.Name, err)
				}
			}

			// The console runs as long as it takes unless --timeout is given.
			ctx, cancel := cmd.Context(), context.CancelFunc(func() {})
			if cmd.Flags().Changed("timeout") {
				ctx, cancel = a.context(cmd)
			}
			defer cancel()

			// The log is plain text; the terminal gets the colours.
			var dst io.Writer = io.MultiWriter(a.Stdout, console.Plain(log))
			if a.output == "json" {
				dst = console.Plain(log)
			}
			match, err := console.Capture(ctx, port, dst, pattern)
			if err != nil {
				return fmt.Errorf("console %s: %w", node.Name, err)
			}
			if a.output == "json" {
				return a.writeJSON(map[string]string{
					"node": node.Name, "device": node.Console.Device, "log": logPath, "match": match,
				})
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&until, "until", "", "stop when the output matches this regular expression")
	cmd.Flags().StringVar(&logPath, "log", "", "log file (default logs/<node>.log)")
	cmd.Flags().StringVar(&send, "send", "", `text to type first, with Go escapes (\r is Enter)`)
	return cmd
}
