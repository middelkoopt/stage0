package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/initrd"
)

func (a *App) buildCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "build", Short: "Build boot artifacts"}
	var out, agentPath, payload string
	ird := &cobra.Command{
		Use:   "initrd --out FILE",
		Short: "Build stage0's initramfs: this binary as /init, which runs the agent",
		Long: "Build a gzip'd initramfs whose /init is stage0 itself (static; it runs as the\n" +
			"agent when it is PID 1), with an optional --payload packed as /payload. The\n" +
			"agent prints a STAGE0-REPORT line on the console with the node's disks and\n" +
			"the payload's SHA-256.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			if out == "" {
				return errors.New("--out is required")
			}
			if agentPath == "" {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				agentPath = exe
			}
			if err := a.CheckStatic(agentPath); err != nil {
				return err
			}
			tmp, err := os.CreateTemp(filepath.Dir(out), ".initrd-*")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			s, err := initrd.Build(tmp, agentPath, payload)
			if err = errors.Join(err, tmp.Close()); err != nil {
				return err
			}
			if err := os.Chmod(tmp.Name(), 0o644); err != nil {
				return err
			}
			if err := os.Rename(tmp.Name(), out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.writeJSON(struct {
					Path string `json:"path"`
					initrd.Summary
				}{out, s})
			}
			msg := fmt.Sprintf("wrote %s: %.1f MB (agent %.1f MB", out, mb(s.Bytes), mb(s.AgentBytes))
			if s.PayloadBytes > 0 {
				msg += fmt.Sprintf(", payload %.1f MB", mb(s.PayloadBytes))
			}
			fmt.Fprintln(a.Stdout, msg+" before compression)")
			return nil
		},
	}
	ird.Flags().StringVar(&out, "out", "", "file to write, e.g. ROOT/initramfs8")
	ird.Flags().StringVar(&agentPath, "agent", "", "static stage0 binary to pack (default: this one)")
	ird.Flags().StringVar(&payload, "payload", "", "file to pack as /payload")
	cmd.AddCommand(ird, a.buildSeedCommand())
	return cmd
}

func (a *App) buildSeedCommand() *cobra.Command {
	var from, out string
	cmd := &cobra.Command{
		Use:   "seed --from DIR --out FILE",
		Short: "Pack first-boot files for an installed image's boot partition",
		Long: "Pack the files under --from (e.g. a cmdline.txt, cloud-init data) as a\n" +
			"seed. Serve it and give the agent stage0.seed=<url>: after writing the image it\n" +
			"unpacks the seed over the image's boot partition (its first, FAT, partition).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			if from == "" || out == "" {
				return errors.New("--from and --out are required")
			}
			tmp, err := os.CreateTemp(filepath.Dir(out), ".seed-*")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			n, err := initrd.BuildSeed(tmp, from)
			if err = errors.Join(err, tmp.Close(), os.Chmod(tmp.Name(), 0o644)); err != nil {
				return err
			}
			if err := os.Rename(tmp.Name(), out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.writeJSON(map[string]any{"path": out, "files": n})
			}
			fmt.Fprintf(a.Stdout, "wrote %s: %d files\n", out, n)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "directory of first-boot files")
	cmd.Flags().StringVar(&out, "out", "", "file to write, e.g. ROOT/seed.cpio")
	return cmd
}

func mb(n int64) float64 { return float64(n) / 1e6 }
