// Package agent is stage0 running on the node, as PID 1 of the initramfs
// stage0 builds. So far it reports what the node has: one line on the
// console, "STAGE0-REPORT {json}", that labctl console --until can wait for.
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

	"github.com/middelkoopt/stage0/internal/bootplan"
)

// Time limits for the agent's waits, in one place; each was set by what
// a node needed.
const (
	diskWait     = 15 * time.Second // the target disk to appear (drivers may still load)
	deviceWait   = 10 * time.Second // a new partition's device node
	ifaceWait    = 10 * time.Second // an Ethernet interface to appear
	dhcpWait     = 30 * time.Second // a DHCP lease, all tries
	dhcpTry      = 3 * time.Second  // one DHCP exchange (5 tries)
	slaacWait    = 60 * time.Second // a global IPv6 address by SLAAC
	reportPost   = 10 * time.Second // posting one report to stage0
	pollInterval = 250 * time.Millisecond

	// The report is taken once the devices settle: quiet for settleQuiet,
	// after at least settleMin, at most settleMax.
	settleQuiet = 500 * time.Millisecond
	settleMin   = 3 * time.Second
	settleMax   = 30 * time.Second

	// Drivers load in passes, coldplugSettle apart, until no new device
	// appears (at most coldplugPasses).
	coldplugSettle = 500 * time.Millisecond
	coldplugPasses = 8
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
	if plan, _ := bootplan.Parse(string(cmdline)); plan.NeedsNetwork() {
		if err := bringUpNetwork(out, plan.IPv6()); err != nil {
			fmt.Fprintf(out, "stage0 agent: network: %v\n", err)
		}
	}
	run(out, string(cmdline))
}

// Hook runs the agent from a dracut hook in a container's installer
// initramfs (docs/containers.md): dracut has mounted the kernel
// filesystems, loaded drivers and brought the network up. Like Main it
// never returns, so dracut never goes on to boot.
func Hook() {
	out := os.Stdout // the hook points it at /dev/console
	fmt.Fprintln(out, "stage0 agent: started from the installer's initramfs")
	cmdline, _ := os.ReadFile("/proc/cmdline")
	run(out, string(cmdline))
}

// run reports, installs as the command line says, then waits.
func run(out io.Writer, cmdline string) {
	plan, perr := bootplan.Parse(cmdline)
	e := emitter{out: out}
	if strings.HasPrefix(plan.Report, "http://") { // even if the rest is wrong
		e.url = plan.Report
	}
	r, err := settle("/", settleQuiet, settleMin, settleMax)
	if err != nil {
		e.emit(kindError, fmt.Appendf(nil, "report: %v", err))
	} else {
		b, _ := json.Marshal(r)
		e.emit(kindReport, b)
	}
	install(e, plan, perr)
	fmt.Fprintln(out, "stage0 agent: done; waiting to be powered off")
	for {
		time.Sleep(time.Hour)
	}
}

// install writes the disk if the plan asks for it; err is the plan's
// parse error.
func install(e emitter, plan bootplan.Plan, err error) {
	out := e.out
	fail := func(what string, err error) { e.emit(kindError, fmt.Appendf(nil, "%s: %v", what, err)) }
	switch {
	case err != nil:
		fail("install", err)
		return
	case !plan.Installs():
		return
	}
	// A card can be found after the first driver pass (a Pi 5's SD card
	// on Ubuntu's kernel, whose mmc_block is a module): load drivers again
	// until the disk is there.
	waitFor("/dev/"+plan.Disk, diskWait, func() { loadModules(io.Discard) })
	var res Result
	if plan.OCI != "" {
		res, err = installContainer(out, plan)
	} else {
		res, err = installImage(out, plan)
	}
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

// installImage writes the disk image (fetched first if plan.URL is set,
// else the payload in the initramfs) to the disk.
func installImage(out io.Writer, plan bootplan.Plan) (Result, error) {
	if plan.URL != "" {
		fmt.Fprintf(out, "stage0 agent: fetching %s\n", plan.URL)
		if _, err := Fetch(plan.URL, "/"+PayloadPath, out); err != nil {
			return Result{}, err
		}
	}
	fmt.Fprintf(out, "stage0 agent: installing the payload on /dev/%s\n", plan.Disk)
	return Install("/"+PayloadPath, "/dev/"+plan.Disk, plan.SHA256, out)
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

// waitDev waits for a device node (a new partition) to appear.
func waitDev(p string) error {
	waitFor(p, deviceWait, func() {})
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("%s did not appear", p)
	}
	return nil
}

// waitFor calls try until path exists or d has passed.
func waitFor(path string, d time.Duration, try func()) {
	for deadline := time.Now().Add(d); ; time.Sleep(pollInterval) {
		if _, err := os.Stat(path); err == nil || time.Now().After(deadline) {
			return
		}
		try()
	}
}
