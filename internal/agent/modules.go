package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// When the agent is packed on top of a distro's initramfs (a VM's EL
// kernel, which builds virtio drivers as modules), nothing loads drivers
// for it: no udev runs. loadModules does udev's coldplug the simple way:
// every device's modalias, handed to the initramfs's own modprobe. With
// stage0's own initramfs there is no modprobe, and nothing to do (a Pi
// kernel has its drivers built in).

// findModprobe returns the modprobe under root, or "".
func findModprobe(root string) string {
	for _, p := range []string{"usr/sbin/modprobe", "sbin/modprobe", "usr/bin/modprobe"} {
		if fi, err := os.Stat(filepath.Join(root, p)); err == nil && fi.Mode()&0o111 != 0 {
			return filepath.Join(root, p)
		}
	}
	return ""
}

// modaliases returns the modalias of every device under dir (/sys/devices).
func modaliases(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // devices come and go; skip what can't be read
		}
		if d.Type()&fs.ModeSymlink != 0 && d.IsDir() {
			return filepath.SkipDir
		}
		if d.Name() != "modalias" || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if a := strings.TrimSpace(string(b)); a != "" {
			out = append(out, a)
		}
		return nil
	})
	return out, err
}

// loadModules loads the drivers the devices under /sys/devices ask for,
// if there is a modprobe.
func loadModules(out io.Writer) {
	mp := findModprobe("/")
	if mp == "" {
		return
	}
	aliases, err := modaliases("/sys/devices")
	if err != nil || len(aliases) == 0 {
		return
	}
	// -b: honour blacklists; -q: aliases with no module are normal.
	cmd := exec.Command(mp, append([]string{"-a", "-b", "-q"}, aliases...)...)
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		fmt.Fprintf(out, "stage0 agent: modprobe: %v\n", err)
		return
	}
	fmt.Fprintf(out, "stage0 agent: loaded drivers for %d devices\n", len(aliases))
}
