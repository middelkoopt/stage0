package cli

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is the build's git revision (with "-dirty" when built from
// uncommitted changes) and commit time, from Go's build info.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, t, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value[:min(len(s.Value), 12)]
		case "vcs.time":
			t = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "unknown (" + bi.Main.Version + ")"
	}
	return fmt.Sprintf("%s%s (%s)", rev, dirty, t)
}

func (a *App) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "This build's git revision",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.output == "json" {
				return a.writeJSON(map[string]string{"version": version()})
			}
			fmt.Fprintln(a.Stdout, version())
			return nil
		},
	}
}

// checkFresh refuses to run from a copy of stage0 (the capped
// /usr/local/bin/stage0) when ./stage0 in dir is newer and different: a
// rebuild without tools/install-capped.sh would otherwise run old code,
// agent included.
func checkFresh(exe, dir string) error {
	local := filepath.Join(dir, "stage0")
	li, err := os.Stat(local)
	if err != nil || !li.Mode().IsRegular() {
		return nil
	}
	ei, err := os.Stat(exe)
	if err != nil || os.SameFile(li, ei) || !li.ModTime().After(ei.ModTime()) {
		return nil
	}
	if same, err := sameContents(local, exe); err != nil || same {
		return err
	}
	return fmt.Errorf("%s is older than %s and differs: refresh it with tools/install-capped.sh", exe, local)
}

func sameContents(a, b string) (bool, error) {
	sum := func(p string) ([]byte, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		h := sha256.New()
		_, err = io.Copy(h, f)
		return h.Sum(nil), err
	}
	sa, err := sum(a)
	if err != nil {
		return false, err
	}
	sb, err := sum(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(sa, sb), nil
}

// checkFreshExe is checkFresh for this binary and the working directory.
func (a *App) checkFreshExe() error {
	if a.Executable == nil { // tests without a binary
		return nil
	}
	exe, err := a.Executable()
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return checkFresh(exe, dir)
}
