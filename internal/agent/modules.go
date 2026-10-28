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
	"time"
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

// modaliases returns the modalias of every device under dir (/sys/devices),
// once each: from its modalias file or, for buses without one (an SD
// card's mmc), the MODALIAS line of its uevent.
func modaliases(dir string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // devices come and go; skip what can't be read
		}
		if d.Type()&fs.ModeSymlink != 0 && d.IsDir() {
			return filepath.SkipDir
		}
		if d.IsDir() || (d.Name() != "modalias" && d.Name() != "uevent") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		a := strings.TrimSpace(string(b))
		if d.Name() == "uevent" {
			a = ""
			for l := range strings.Lines(string(b)) {
				if v, ok := strings.CutPrefix(l, "MODALIAS="); ok {
					a = strings.TrimSpace(v)
				}
			}
		}
		if a != "" && !seen[a] {
			seen[a] = true
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
	scan := func() []string {
		a, _ := modaliases("/sys/devices")
		return a
	}
	load := func(aliases []string) {
		// -b: honour blacklists; -q: aliases with no module are normal.
		cmd := exec.Command(mp, append([]string{"-a", "-b", "-q"}, aliases...)...)
		cmd.Stdout, cmd.Stderr = out, out
		var ee *exec.ExitError
		if err := cmd.Run(); err != nil && !errors.As(err, &ee) {
			fmt.Fprintf(out, "stage0 agent: modprobe: %v\n", err)
		}
	}
	if n := coldplug(scan, load, func() { time.Sleep(coldplugSettle) }); n > 0 {
		fmt.Fprintf(out, "stage0 agent: loaded drivers for %d devices\n", n)
	}
}

// coldplug loads drivers for the devices scan finds, again for those that
// appear as drivers load (a Pi 5's Ethernet and SD card sit behind RP1, a
// PCIe device with its own driver), until no new ones turn up. It returns
// how many devices it asked for.
func coldplug(scan func() []string, load func([]string), settle func()) int {
	seen := map[string]bool{}
	for range coldplugPasses {
		var add []string
		for _, a := range scan() {
			if !seen[a] {
				seen[a] = true
				add = append(add, a)
			}
		}
		if len(add) == 0 {
			break
		}
		load(add)
		settle()
	}
	return len(seen)
}
