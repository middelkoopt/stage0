// Package agent is stage0 running on the node, as PID 1 of the initramfs
// stage0 builds. So far it reports what the node has: one line on the
// console, "STAGE0-REPORT {json}", that stage0 console --until can wait for.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReportPrefix starts the report line on the console.
const ReportPrefix = "STAGE0-REPORT "

// PayloadPath is where stage0 build initrd puts the payload.
const PayloadPath = "payload"

// Report is what the node has.
type Report struct {
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	Kernel     string `json:"kernel"`
	Cmdline    string `json:"cmdline"`
	MemTotalMB int    `json:"memTotalMB"`
	// Filesystems are the disk filesystems the kernel can mount.
	Filesystems []string    `json:"filesystems"`
	Disks       []Disk      `json:"disks"`
	Interfaces  []Interface `json:"interfaces"`
	Payload     *Payload    `json:"payload,omitempty"`
}

// Disk is a whole block device.
type Disk struct {
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	Model     string `json:"model,omitempty"`
	Removable bool   `json:"removable,omitempty"`
}

// Interface is a network interface other than loopback.
type Interface struct {
	Name  string `json:"name"`
	MAC   string `json:"mac"`
	State string `json:"state"`
}

// Payload is the file packed with the agent, checked on arrival.
type Payload struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Main runs the agent as PID 1: mount the kernel filesystems, report, then
// wait to be powered off. PID 1 must never exit (the kernel panics), so
// errors are printed and it waits anyway.
func Main() {
	out := os.Stdout // the kernel's /dev/console
	fmt.Fprintln(out, "stage0 agent: started")
	for _, m := range []struct{ fstype, dir string }{{"proc", "/proc"}, {"sysfs", "/sys"}, {"devtmpfs", "/dev"}} {
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			fmt.Fprintf(out, "STAGE0-ERROR mkdir %s: %v\n", m.dir, err)
		}
		if err := syscall.Mount(m.fstype, m.dir, m.fstype, 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
			fmt.Fprintf(out, "STAGE0-ERROR mount %s: %v\n", m.dir, err)
		}
	}
	loadModules(out)
	cmdline, _ := os.ReadFile("/proc/cmdline")
	if needsNetwork(string(cmdline)) {
		if err := bringUpNetwork(out); err != nil {
			fmt.Fprintf(out, "stage0 agent: network: %v\n", err)
		}
	}
	e := emitter{out: out, url: reportURL(string(cmdline))}
	r, err := settle("/", 500*time.Millisecond, 3*time.Second, 30*time.Second)
	if err != nil {
		e.emit(kindError, fmt.Appendf(nil, "report: %v", err))
	} else {
		b, _ := json.Marshal(r)
		e.emit(kindReport, b)
	}
	install(e, string(cmdline))
	fmt.Fprintln(out, "stage0 agent: done; waiting to be powered off")
	for {
		time.Sleep(time.Hour)
	}
}

// install writes the disk if the command line asks for it.
func install(e emitter, cmdline string) {
	out := e.out
	fail := func(what string, err error) { e.emit(kindError, fmt.Appendf(nil, "%s: %v", what, err)) }
	plan, err := ParsePlan(cmdline)
	switch {
	case err != nil:
		fail("install", err)
		return
	case plan == nil:
		return
	}
	if plan.URL != "" {
		fmt.Fprintf(out, "stage0 agent: fetching %s\n", plan.URL)
		if _, err := Fetch(plan.URL, "/"+PayloadPath, out); err != nil {
			fail("install", err)
			return
		}
	}
	fmt.Fprintf(out, "stage0 agent: installing the payload on /dev/%s\n", plan.Disk)
	res, err := Install("/"+PayloadPath, "/dev/"+plan.Disk, plan.SHA256, out)
	if err != nil {
		fail("install", err)
		return
	}
	if plan.Seed != "" {
		fmt.Fprintf(out, "stage0 agent: fetching first-boot files %s\n", plan.Seed)
		const seed = "/seed.cpio"
		if _, err := Fetch(plan.Seed, seed, out); err != nil {
			fail("seed", err)
			return
		}
		if res.Seeded, err = seedDisk(plan.Disk, seed); err != nil {
			fail("seed", err)
			return
		}
	}
	b, _ := json.Marshal(res)
	e.emit(kindInstalled, b)
}

// needsNetwork reports whether the command line asks for anything over
// the network.
func needsNetwork(cmdline string) bool {
	for _, f := range strings.Fields(cmdline) {
		for _, p := range []string{"stage0.url=", "stage0.seed=", "stage0.report="} {
			if strings.HasPrefix(f, p) {
				return true
			}
		}
	}
	return false
}

// settle gathers until the disk list has stayed the same for quiet (disks
// appear a moment after boot), polling every poll, giving up after limit.
func settle(root string, poll, quiet, limit time.Duration) (Report, error) {
	deadline := time.Now().Add(limit)
	r, err := Gather(root)
	if err != nil {
		return r, err
	}
	stable := time.Now()
	for time.Since(stable) < quiet && time.Now().Before(deadline) {
		time.Sleep(poll)
		next, err := Gather(root)
		if err != nil {
			return next, err
		}
		if !reflect.DeepEqual(next.Disks, r.Disks) {
			stable = time.Now()
		}
		r = next
	}
	return r, nil
}

// Gather reads the report from the /proc and /sys under root.
func Gather(root string) (Report, error) {
	var r Report
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
	}
	r.Model = read("sys/firmware/devicetree/base/model")
	r.Serial = read("sys/firmware/devicetree/base/serial-number")
	r.Kernel = read("proc/sys/kernel/osrelease")
	r.Cmdline = read("proc/cmdline")
	for line := range strings.Lines(read("proc/meminfo")) {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			r.MemTotalMB = kb / 1024
		}
	}

	for line := range strings.Lines(read("proc/filesystems")) {
		if f := strings.Fields(line); len(f) == 1 {
			r.Filesystems = append(r.Filesystems, f[0])
		}
	}

	blocks, err := os.ReadDir(filepath.Join(root, "sys/block"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return r, err
	}
	for _, e := range blocks {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}
		dir := filepath.Join("sys/block", name)
		sectors, _ := strconv.ParseInt(read(filepath.Join(dir, "size")), 10, 64)
		if sectors == 0 {
			continue // empty card reader and the like
		}
		model := read(filepath.Join(dir, "device/model"))
		if model == "" {
			model = read(filepath.Join(dir, "device/name")) // SD and eMMC
		}
		r.Disks = append(r.Disks, Disk{
			Name: name, Bytes: sectors * 512, Model: model,
			Removable: read(filepath.Join(dir, "removable")) == "1",
		})
	}

	ifs, err := os.ReadDir(filepath.Join(root, "sys/class/net"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return r, err
	}
	for _, e := range ifs {
		if e.Name() == "lo" {
			continue
		}
		dir := filepath.Join("sys/class/net", e.Name())
		r.Interfaces = append(r.Interfaces, Interface{Name: e.Name(), MAC: read(filepath.Join(dir, "address")), State: read(filepath.Join(dir, "operstate"))})
	}
	slices.SortFunc(r.Disks, func(a, b Disk) int { return strings.Compare(a.Name, b.Name) })

	p, err := payload(filepath.Join(root, PayloadPath))
	if err != nil {
		return r, err
	}
	r.Payload = p
	return r, nil
}

func payload(path string) (*Payload, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	return &Payload{Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
