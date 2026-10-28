package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/bootplan"
	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/console"
	"github.com/middelkoopt/stage0/internal/container"
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/image"
	"github.com/middelkoopt/stage0/internal/initrd"
	"github.com/middelkoopt/stage0/internal/oci"
	"github.com/middelkoopt/stage0/internal/reach"
	"github.com/middelkoopt/stage0/internal/registry"
	"github.com/middelkoopt/stage0/internal/tftpd"
	"github.com/middelkoopt/stage0/internal/uki"
)

// Time limits for the waits, unless --timeout is given for the whole.
const (
	agentWait = 20 * time.Minute // power on to STAGE0-INSTALLED
	reachTry  = 15 * time.Second // one ping-and-SSH try at an address
)

// readyWait is the time from the power cycle to the installed system
// answering SSH; leaseGrace how long, in mode own, its lease is waited for
// before its link-local address will do (variables so tests can shorten
// them).
var (
	readyWait  = 10 * time.Minute
	leaseGrace = time.Minute
	upPoll     = 3 * time.Second // between looks for the installed system
)

// initrdName is stage0's initramfs in the served boot files, named so it
// can't clash with the image's own.
const initrdName = "stage0-initrd"

// installResult is install's JSON output.
type installResult struct {
	Node      string          `json:"node"`
	Port      int             `json:"port"`
	Image     string          `json:"image"`
	Disk      string          `json:"disk"`
	Report    json.RawMessage `json:"report,omitempty"`
	Installed json.RawMessage `json:"installed,omitempty"`
	Ready     string          `json:"ready,omitempty"`
	Seconds   float64         `json:"seconds"`
	Address   string          `json:"address,omitempty"` // IPv6, found on the network after boot
	Network   string          `json:"network"`
	MAC       string          `json:"mac,omitempty"`   // the interface it netbooted on
	IPv4      string          `json:"ipv4,omitempty"`  // its lease from stage0 on Network
	Reach     *reach.Result   `json:"reach,omitempty"` // ping and SSH to IPv4 after boot
	Log       string          `json:"log"`
	ServeLog  string          `json:"serveLog"`
}

func (a *App) installCommand() *cobra.Command {
	var imageName, imagesDir, cacheDir, disk, network string
	var setPort, keep, ipv6 bool
	cmd := &cobra.Command{
		Use:   "install <node> --image NAME",
		Short: "Install an image on a node: netboot the agent, write the disk, boot it",
		Long: "Install the image from images/NAME.yaml on a node and boot it:\n" +
			"  download and check the image, and stage its boot files with stage0's agent;\n" +
			"  serve DHCP, TFTP and HTTP on the node's network (site.yaml);\n" +
			"  power the node off and on, and follow its console while the agent writes\n" +
			"  the disk; stop serving, power it off and on again, and wait for the\n" +
			"  installed system's prompt.\n" +
			"The node's switch port must be on the network's VLAN: --set-port moves it\n" +
			"there (stage0 set port does it by hand). Console output goes to logs/<node>.log, serve's to logs/<node>-serve.log.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkOutput(); err != nil {
				return err
			}
			if err := a.checkFreshExe(); err != nil {
				return err
			}
			if imageName == "" {
				return errors.New("--image is required (a file in " + imagesDir + "/)")
			}
			if cacheDir == "" {
				d, err := os.UserCacheDir()
				if err != nil {
					return err
				}
				cacheDir = filepath.Join(d, "stage0")
			}
			ctx := cmd.Context()
			if cmd.Flags().Changed("timeout") {
				var cancel context.CancelFunc
				ctx, cancel = a.context(cmd)
				defer cancel()
			}
			return a.install(ctx, installOptions{
				node: args[0], image: imageName, images: imagesDir, cache: cacheDir, disk: disk, network: network,
				setPort: setPort, keep: keep, ipv6: ipv6,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&imageName, "image", "", "image to install: images/NAME.yaml")
	f.StringVar(&imagesDir, "images", "images", "directory of image config files")
	f.StringVar(&cacheDir, "cache", "", "where images and staged boot files go (default: the user cache dir/stage0)")
	f.StringVar(&disk, "disk", "", "disk to write, e.g. mmcblk0 (default: the node's disk in site.yaml)")
	f.StringVar(&network, "network", "", "network to install over (default: the only one in site.yaml)")
	f.BoolVar(&ipv6, "ipv6", false, "install over IPv6 alone: the agent gets its address by SLAAC and fetches from <prefix>::1 (needs the network's prefix6, and the CAP_NET_RAW/CAP_NET_ADMIN copy)")
	f.BoolVar(&keep, "keep", false, "after the install, keep serving (and a VM running) until interrupted or --timeout")
	f.BoolVar(&setPort, "set-port", false, "move the node's port to the network's VLAN first (its old PVID stays tagged)")
	return cmd
}

type installOptions struct {
	node, image, images, cache, disk, network string
	setPort, keep, ipv6                       bool
}

func (a *App) install(ctx context.Context, o installOptions) error {
	start := time.Now()
	say := func(format string, v ...any) {
		if a.output == "text" {
			fmt.Fprintf(a.Stdout, format+"\n", v...)
		}
	}

	// Everything is checked before anything is downloaded or powered.
	site, err := config.LoadSite(a.sitePath)
	if err != nil {
		return err
	}
	node, err := site.Node(o.node)
	if err != nil {
		return err
	}
	disk := o.disk
	if disk == "" {
		disk = node.Disk
	}
	if disk == "" {
		return fmt.Errorf("no disk for %s: give --disk or set disk: in %s", node.Name, a.sitePath)
	}
	network := cmp.Or(o.network, node.Network)
	n, err := oneNetwork(site, network)
	if err != nil {
		return err
	}
	img, err := image.Load(o.images, o.image)
	if err != nil {
		return err
	}
	switch {
	case node.VM != nil && !img.VM():
		return fmt.Errorf("%s is a VM, but image %s has no agent kernel for VMs", node.Name, img.Name)
	case node.VM == nil && img.VM():
		return fmt.Errorf("image %s is for VMs, but %s is not one", img.Name, node.Name)
	}
	switch {
	case o.ipv6 && !n.Server6.IsValid():
		return fmt.Errorf("--ipv6: network %q has no prefix6", n.Name)
	case node.VM != nil && img.Container != "":
		return errors.New("containers install on Pis only, for now")
	}
	fb := resolveFirstBoot(node, img)
	if fb.Mode == "ignition" && img.Container == "" {
		return fmt.Errorf("%s: firstBoot: ignition is for containers", node.Name)
	}
	ready, _ := regexp.Compile(img.Ready) // checked by Load
	var installed string
	if !img.VM() {
		if fb.Mode != "stage0" { // the chosen tool sets hostname and keys
			installed = strings.TrimSpace(img.Cmdline)
		} else if installed, err = installedCmdline(img.Cmdline, node.Hostname, site.SSHKeys); err != nil {
			return err
		}
	}
	var poe *poeMachine
	if node.VM == nil {
		if poe, err = a.newPoEMachine(ctx, node, n, o.setPort, say); err != nil {
			return err
		}
	}

	// Stage the boot files.
	progress := io.Discard // -o json prints only the result
	if a.output == "text" {
		progress = a.Stdout
	}
	secrets, err := config.LoadSecrets(a.secretsPath)
	if err != nil {
		return err
	}
	creds := map[string]registry.Credentials{}
	for host, r := range secrets.Registries {
		creds[host] = registry.Credentials{User: r.User, Password: r.Password}
	}
	imgPath, err := image.FetchWith(ctx, img, o.cache, progress, creds)
	if err != nil {
		return err
	}
	say("stage0 %s; image %s: %s", version(), img.Name, imgPath)
	run := filepath.Join(o.cache, "run", node.Name)
	if img.VM() {
		// Over IPv6 the agent takes its address by SLAAC itself.
		base := "http://" + n.Server.String()
		if o.ipv6 {
			base = "http://[" + n.Server6.String() + "]"
		}
		cmdline := "console=ttyAMA0 " + bootplan.Plan{Disk: disk, SHA256: img.SHA256,
			URL: base + "/images/" + img.File(), Report: base + "/stage0/report"}.Encode()
		err = a.stageVM(ctx, run, img, imgPath, node, site.SSHKeys, fb, cmdline, o.cache, progress)
	} else {
		server := n.Server.String()
		if o.ipv6 {
			server = "[" + n.Server6.String() + "]"
		}
		if img.Container != "" {
			err = a.stageContainer(run, imgPath, img, node, fb, installed, disk, server, filepath.Join(o.cache, "boot"))
		} else {
			err = a.stage(run, imgPath, img, fb, installed, disk, server, filepath.Join(o.cache, "boot", img.SHA256))
		}
	}
	if err != nil {
		return err
	}
	say("staged boot files in %s", run)

	// Logs.
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return err
	}
	res := installResult{Node: node.Name, Image: img.Name, Disk: disk, Network: n.Name,
		Log: filepath.Join("logs", node.Name+".log"), ServeLog: filepath.Join("logs", node.Name+"-serve.log")}
	consoleLog, err := os.OpenFile(res.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer consoleLog.Close()
	serveLog, err := os.OpenFile(res.ServeLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer serveLog.Close()

	// The agent's progress comes over HTTP and, with a serial console,
	// from the console too, followed from before power-on to the end.
	cctx, stopConsole := context.WithCancel(ctx)
	defer stopConsole()
	w := newWatcher(cctx, ready)
	var consoles []io.Closer
	defer func() {
		for _, c := range consoles {
			c.Close()
		}
	}()
	follow := func(c io.ReadWriteCloser) {
		consoles = append(consoles, c)
		fmt.Fprintf(consoleLog, "\n=== stage0 install %s %s %s ===\n", node.Name, img.Name, time.Now().Format(time.RFC3339))
		go console.Capture(cctx, c, console.Plain(io.MultiWriter(consoleLog, w)), nil)
	}
	if node.Console != nil {
		portIO, err := a.OpenConsole(node.Console.Device, node.Console.Baud)
		if err != nil {
			return err
		}
		follow(portIO)
	}
	report := func(from, kind string, body []byte) {
		w.send(event{kind: map[string]string{"report": agent.ReportPrefix, "installed": agent.InstalledPrefix, "error": errorPrefix}[kind], text: string(body), http: true})
	}

	// Serve for the whole install: the agent, then the installed system's
	// DHCP. Other installed nodes keep their addresses.
	others, err := a.installedOn(n, node.Name)
	if err != nil {
		return err
	}
	slog := slog.New(slog.NewTextHandler(serveLog, nil)).With("network", n.Name)
	leases := a.newLeases(n, others, slog)
	if o.ipv6 && node.VM != nil {
		// UEFI tries IPv4 first: leave that unanswered.
		hw, _ := net.ParseMAC(node.VM.MAC)
		leases.BootIPv6(hw)
	}

	// The machine: a Pi on its port, or a VM on the test bed's switch,
	// which install runs while it works.
	var m machine = poe
	if node.VM != nil {
		vmDir := filepath.Join(o.cache, "vm")
		sock := filepath.Join(vmDir, n.Name+".sock")
		if err := os.MkdirAll(vmDir, 0o755); err != nil {
			return err
		}
		if err := a.runSwitch(cctx, sock, n.Interface, slog); err != nil {
			return err
		}
		firmware := node.VM.Firmware
		if firmware == "" {
			if firmware, err = image.FetchDebFile(ctx, uefiFirmware, uefiFirmwareCode, filepath.Join(o.cache, "firmware"), progress); err != nil {
				return err
			}
			if _, err = image.FetchDebFile(ctx, uefiFirmware, uefiFirmwareVars, filepath.Join(o.cache, "firmware"), progress); err != nil {
				return err
			}
		}
		vm, err := newVMMachine(a, node, firmware, vmDir, sock, serveLog)
		if err != nil {
			return err
		}
		base := "http://" + n.Server.String()
		if img.NoCloud {
			vm.serial = "ds=nocloud;s=" + base + "/nocloud/" + node.Name + "/"
		}
		m = vm
		defer vm.off(ctx)
	}
	res.Port = m.port()
	// TFTP stops offering files once the node is installed (mode none
	// leaves no DHCP to withhold, so its netboot would find them again).
	files, err := tftpd.New(run, slog)
	if err != nil {
		return err
	}
	sctx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = a.serve(sctx, n, run, leases, files, report, slog)
		close(serveDone)
	}()
	defer func() { stopServe(); <-serveDone }()

	if err := m.bootAgent(ctx, follow); err != nil {
		return err
	}
	say("%s: powered on, booting stage0's agent", m.name())

	actx, cancel := context.WithTimeout(ctx, agentWait)
	defer cancel()
	reportByHTTP := false
	for done := false; !done; {
		select {
		case <-actx.Done():
			return fmt.Errorf("%s: the agent did not report STAGE0-INSTALLED (logs: %s, %s): %w", node.Name, res.Log, res.ServeLog, actx.Err())
		case <-serveDone:
			return fmt.Errorf("serve stopped: %v (log: %s)", serveErr, res.ServeLog)
		case e := <-w.events:
			switch e.kind {
			// The report and the result come by HTTP and by console. HTTP
			// is the authority: a console copy counts only until an HTTP
			// one arrives, and never replaces it (other output can garble
			// the console's: systemd's status lines in an installer). Only
			// valid JSON counts either way.
			case agent.ReportPrefix:
				if !takeReport(e, reportByHTTP) {
					continue
				}
				if res.Report == nil {
					say("%s: agent running", node.Name)
				}
				res.Report, reportByHTTP = json.RawMessage(e.text), reportByHTTP || e.http
			case agent.InstalledPrefix:
				if !json.Valid([]byte(e.text)) {
					continue // the other copy follows
				}
				res.Installed = json.RawMessage(e.text)
				var r agent.Result
				json.Unmarshal(res.Installed, &r)
				say("%s: wrote %.1f GB to %s in %.0f s", node.Name, float64(r.Bytes)/1e9, r.Disk, r.Seconds)
				done = true
			case errorPrefix:
				if err := m.off(ctx); err != nil {
					say("%s: powering off: %v", node.Name, err)
				}
				return fmt.Errorf("%s: agent: %s (node powered off; logs: %s, %s)", node.Name, e.text, res.Log, res.ServeLog)
			}
		}
	}

	files.Close()
	// From now on its PXE requests get no answer, so it boots from its
	// disk; its installed system's DHCP gets the same address.
	if macs, err := res.macs(); err == nil {
		for _, m := range macs {
			ip, ok := leases.Install(m)
			if !ok && o.ipv6 { // it netbooted over IPv6: no IPv4 lease yet
				ip, ok = leases.Reserve(m)
			}
			if ok {
				res.MAC = m.String()
				if ip.IsValid() { // none in proxy mode: the other server leases
					res.IPv4 = ip.String()
				}
				break
			}
		}
	}
	w.armReady()
	if err := m.bootDisk(ctx, follow); err != nil {
		return err
	}
	say("%s: rebooting from %s", node.Name, disk)
	if err := a.waitUp(ctx, &res, node, n, leases, w, start, say); err != nil {
		return err
	}
	if err := a.saveRecord(res); err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, addressWait)
	defer cancel()
	if res.Address != "" {
		// found while waiting for it
	} else if addr, err := a.address(fctx, res, n); err != nil {
		say("%s: no IPv6 address found yet (%v); try: stage0 get address %s", node.Name, err, node.Name)
	} else {
		res.Address = addr
		say("%s: %s\n  ssh root@%s", node.Name, addr, addr)
		if err := a.saveRecord(res); err != nil { // now with the address
			return err
		}
	}
	if a.output == "json" {
		if err := a.writeJSON(res); err != nil {
			return err
		}
	}
	if o.keep {
		say("%s: keeping it served%s until interrupted", node.Name, map[bool]string{true: " and running", false: ""}[node.VM != nil])
		select {
		case <-ctx.Done(): // interrupted, or --timeout: the install itself succeeded
		case <-serveDone:
			return fmt.Errorf("serve stopped: %v (log: %s)", serveErr, res.ServeLog)
		}
	}
	return nil
}

// oneNetwork returns the named network, or the only one.
func oneNetwork(site *config.Site, name string) (config.Network, error) {
	if name != "" {
		return site.Network(name)
	}
	if len(site.Networks) != 1 {
		return config.Network{}, fmt.Errorf("site config has %d networks; name one with --network", len(site.Networks))
	}
	return site.Networks[0], nil
}

// stage builds the directory serve offers the node: the image's boot files
// with stage0's initramfs added to config.txt, a cmdline.txt carrying the
// agent's plan, the seed, and the image itself under images/.
func (a *App) stage(run, imgPath string, img image.Config, fb firstBoot, installed, disk, server, bootCache string) error {
	if _, err := os.Stat(bootCache); err != nil {
		tmp := bootCache + ".tmp"
		os.RemoveAll(tmp)
		if err := image.BootFiles(imgPath, img.BootPartition, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, bootCache); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(run); err != nil {
		return err
	}
	if err := os.CopyFS(run, os.DirFS(bootCache)); err != nil {
		return err
	}

	cfg, err := os.ReadFile(filepath.Join(run, "config.txt"))
	if err != nil {
		return fmt.Errorf("image %s: no config.txt in its boot partition: %w", img.Name, err)
	}
	// The agent boots with stage0's initramfs, on the serial console
	// (enable_uart: a Pi 4's GPIO serial port is off without it). Over
	// IPv6 it needs the kernel's ipv6 module, so it boots kernel8.img
	// with the image's own initramfs8 (modules included) and stage0's
	// appended to it; an image's initramfs setting does the same always.
	ipv6 := strings.HasPrefix(server, "[")
	agentLines := append(slices.Clone(img.Config), "initramfs "+initrdName+" followkernel", "enable_uart=1")
	distroInitrd := img.Initramfs
	if ipv6 && distroInitrd == "" {
		agentLines = append(agentLines, "kernel=kernel8.img")
		distroInitrd = "initramfs8"
	}
	agentCfg := image.EnsureConfig(cfg, agentLines)
	if err := os.WriteFile(filepath.Join(run, "config.txt"), agentCfg, 0o644); err != nil {
		return err
	}
	base := "http://" + server
	netcfg := "ip=dhcp " // the kernel takes an IPv4 address
	if strings.HasPrefix(server, "[") {
		netcfg = "" // IPv6: the agent waits for SLAAC itself
	}
	// serial0 is the Pi's console UART.
	cmdline := "console=serial0,115200 " + netcfg + bootplan.Plan{Disk: disk, SHA256: img.SHA256,
		URL: base + "/images/" + img.File(), Seed: base + "/seed.cpio", Report: base + "/stage0/report"}.Encode() + "\n"
	// The firmware reads cmdline.txt and initramfs under the image's boot
	// directory (os_prefix).
	if err := os.WriteFile(filepath.Join(run, img.BootDir, "cmdline.txt"), []byte(cmdline), 0o644); err != nil {
		return err
	}

	exe, err := a.Executable()
	if err != nil {
		return err
	}
	if err := a.CheckStatic(exe); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(run, img.BootDir, initrdName), func(w io.Writer) error {
		if distroInitrd != "" {
			distro, err := os.Open(filepath.Join(run, distroInitrd))
			if err != nil {
				return fmt.Errorf("image %s: the agent needs its %s: %w", img.Name, distroInitrd, err)
			}
			defer distro.Close()
			n, err := io.Copy(w, distro)
			if err != nil {
				return err
			}
			// The kernel skips zeros between archives and takes an
			// uncompressed cpio only at a 4-byte boundary (k52: a
			// 37,939,845-byte dracut image followed directly by one was
			// "not initramfs").
			if _, err := w.Write(make([]byte, (4-n%4)%4)); err != nil {
				return err
			}
		}
		_, err := initrd.Build(w, exe, "")
		return err
	}); err != nil {
		return err
	}

	if err := writeSeed(run, img, cfg, fb, installed, bootCache); err != nil {
		return err
	}

	images := filepath.Join(run, "images")
	if err := os.Mkdir(images, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(images, img.File())
	if err := os.Link(imgPath, dst); err == nil {
		return nil
	}
	// Another filesystem: copy (serve won't follow a link out of its root).
	src, err := os.Open(imgPath)
	if err != nil {
		return err
	}
	defer src.Close()
	return writeFile(dst, func(w io.Writer) error {
		_, err := io.Copy(w, src)
		return err
	})
}

// stageVM builds a VM's boot root (the image under images/, the node's
// cloud-init NoCloud files under nocloud/<node>/) and the agent's boot: the
// image's kernel, and its initrd with stage0's initramfs appended (the
// kernel unpacks both; stage0's /init wins).
func (a *App) stageVM(ctx context.Context, run string, img image.Config, imgPath string, node config.Node, keys []string,
	fb firstBoot, cmdline, cache string, progress io.Writer) error {
	agentCache := filepath.Join(cache, "agent", img.Name)
	kernel, err := image.FetchFile(ctx, *img.Kernel, agentCache, progress)
	if err != nil {
		return err
	}
	distroInitrd, err := image.FetchFile(ctx, *img.Initrd, agentCache, progress)
	if err != nil {
		return err
	}
	stub, err := image.FetchDebFile(ctx, ukiStub, ukiStubPath, filepath.Join(cache, "uki"), progress)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(run); err != nil {
		return err
	}
	images := filepath.Join(run, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return err
	}
	if err := linkOrCopy(imgPath, filepath.Join(images, img.File())); err != nil {
		return err
	}
	nc := filepath.Join(run, "nocloud", node.Name)
	if err := os.MkdirAll(nc, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{"user-data": []byte(nocloudUserData(node.Hostname, keys)), "meta-data": fb.noCloud()["meta-data"]}
	if fb.Mode == "cloud-init" {
		files = fb.noCloud()
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(nc, name), body, 0o644); err != nil {
			return err
		}
	}

	exe, err := a.Executable()
	if err != nil {
		return err
	}
	if err := a.CheckStatic(exe); err != nil {
		return err
	}
	// The UKI the firmware netboots: the distro's kernel, its initrd with
	// the agent appended (padded: the kernel takes an appended archive at
	// a 4-byte boundary), and the agent's command line.
	var rd bytes.Buffer
	f, err := os.Open(distroInitrd)
	if err != nil {
		return err
	}
	n, err := io.Copy(&rd, f)
	f.Close()
	if err != nil {
		return err
	}
	rd.Write(make([]byte, (4-n%4)%4))
	if _, err := initrd.Build(&rd, exe, ""); err != nil {
		return err
	}
	k, err := os.ReadFile(kernel)
	if err != nil {
		return err
	}
	if k, err = uki.Kernel(k); err != nil {
		return fmt.Errorf("image %s: kernel: %w", img.Name, err)
	}
	s, err := os.ReadFile(stub)
	if err != nil {
		return err
	}
	b, err := uki.Build(s, uki.Parts{Linux: k, Initrd: rd.Bytes(), Cmdline: cmdline,
		OSRel: "NAME=\"stage0 agent\"\nID=stage0\n"})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(run, dhcpd.UEFIBootFile), b, 0o644)
}

// nocloudUserData is cloud-init user-data giving the node its hostname and
// root the site's SSH keys.
func nocloudUserData(hostname string, keys []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#cloud-config\nhostname: %s\ndisable_root: false\nusers:\n  - name: root\n    ssh_authorized_keys:\n", hostname)
	for _, k := range keys {
		fmt.Fprintf(&b, "      - %s\n", k)
	}
	return b.String()
}

// writeSeed writes run/seed.cpio, the first-boot files the agent lays on
// the installed boot partition: cmdline.txt (in the image's boot
// directory), config.txt with the image's lines ensured (cfg is the
// shipped one), its bootFiles, and cloud-init user-data with the hostname
// when the boot partition (bootCache) has user-data.
func writeSeed(run string, img image.Config, cfg []byte, fb firstBoot, installed, bootCache string) error {
	seed := filepath.Join(run, ".seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(seed, img.BootDir), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(seed, img.BootDir, "cmdline.txt"), []byte(installed+"\n"), 0o644); err != nil {
		return err
	}
	if len(img.Config) > 0 {
		if err := os.WriteFile(filepath.Join(seed, "config.txt"), image.EnsureConfig(cfg, img.Config), 0o644); err != nil {
			return err
		}
	}
	for name, body := range img.BootFiles {
		p := filepath.Join(seed, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	switch {
	case fb.Mode == "cloud-init" && img.Container == "":
		// The image's cloud-init reads NoCloud from the boot partition
		// (Ubuntu, Raspberry Pi OS, Alma): the user's data replaces its own.
		for name, body := range fb.noCloud() {
			if err := os.WriteFile(filepath.Join(seed, name), body, 0o644); err != nil {
				return err
			}
		}
	case fb.Mode == "stage0":
		// cloud-init images (user-data on the boot partition) would
		// otherwise set their own hostname at first boot.
		if ud, err := os.ReadFile(filepath.Join(bootCache, "user-data")); err == nil {
			if err := os.WriteFile(filepath.Join(seed, "user-data"), image.SetCloudInitHostname(ud, fb.Hostname), 0o644); err != nil {
				return err
			}
		}
	}
	if err := writeFile(filepath.Join(run, "seed.cpio"), func(w io.Writer) error {
		_, err := initrd.BuildSeed(w, seed)
		return err
	}); err != nil {
		return err
	}
	return os.RemoveAll(seed)
}

// stageContainer builds the directory serve offers a node installing a
// container (docs/containers.md): the container's boot partition, with its
// kernel and its installer initramfs (stage0's agent and dracut hook
// appended) in config.txt, a cmdline.txt carrying the agent's plan, the
// seed, and the OCI layout under oci/ for the agent to pull.
func (a *App) stageContainer(run, layout string, img image.Config, node config.Node, fb firstBoot, installed, disk, server, bootCaches string) error {
	ci, err := oci.Open(layout)
	if err != nil {
		return err
	}
	// The layout: the node's, else the image config's, else the labels'.
	spec, ign, err := container.Resolve(ci.Labels, container.Layout{Partitions: img.Partitions, Ignition: img.IgnitionConfig},
		img.Installer, container.Layout{Partitions: node.Partitions, Ignition: node.IgnitionConfig})
	if err != nil {
		return fmt.Errorf("container %s for %s: %w", img.Container, node.Name, err)
	}
	if fb.Mode == "ignition" && ign == nil {
		return fmt.Errorf("container %s for %s: firstBoot: ignition needs an Ignition config (ignition: on the node or image, or the pi.provision.ignition label)", img.Container, node.Name)
	}
	switch fb.Mode {
	case "ignition":
		spec.FirstBoot = fb.Mode
	case "cloud-init":
		nc := fb.noCloud()
		spec.FirstBoot = fb.Mode
		spec.CloudInit = &container.CloudInit{UserData: string(nc["user-data"]), MetaData: string(nc["meta-data"]),
			NetworkConfig: string(nc["network-config"])}
	}
	digest := strings.TrimPrefix(ci.Digest, "sha256:")
	bootCache := filepath.Join(bootCaches, digest)
	if _, err := os.Stat(bootCache); err != nil {
		tmp := bootCache + ".tmp"
		os.RemoveAll(tmp)
		if err := ci.Extract(spec.Boot().Path, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, bootCache); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(run); err != nil {
		return err
	}
	if err := os.CopyFS(run, os.DirFS(bootCache)); err != nil {
		return err
	}

	kernel, err := spec.InBoot(spec.Installer.Kernel)
	if err != nil {
		return fmt.Errorf("container %s: installer kernel: %w", img.Container, err)
	}
	cfg, err := os.ReadFile(filepath.Join(run, "config.txt"))
	if err != nil {
		return fmt.Errorf("container %s: no config.txt in its boot partition: %w", img.Container, err)
	}
	agentCfg := image.EnsureConfig(cfg, append(slices.Clone(img.Config),
		"kernel="+kernel, "initramfs "+initrdName+" followkernel", "enable_uart=1"))
	if err := os.WriteFile(filepath.Join(run, "config.txt"), agentCfg, 0o644); err != nil {
		return err
	}
	// dracut brings the network up, asking DHCP as the agent does, then
	// its hook runs the agent. No ip=: a Pi kernel with its own DHCP
	// client built in would take it first, without the agent's class, and
	// retry for minutes (k52, Rocky). Over IPv6 (server "[…]") the
	// installer takes its address by SLAAC, and without the agent's class
	// stage0's DHCPv4 leaves it alone.
	base := "http://" + server
	dhcpClass := "rd.net.dhcp.vendor-class=" + dhcpd.AgentClass + " "
	if strings.HasPrefix(server, "[") {
		dhcpClass = ""
	}
	// With an Ignition config, the installer's Ignition partitions and
	// formats the disk first (docs/containers.md).
	ignition := ""
	if spec.Ignition || fb.Mode == "ignition" {
		ignition = "ignition.firstboot ignition.platform.id=metal ignition.config.url=" + base + "/ignition.ign "
		b, err := container.ForDisk(ign, "/dev/"+disk)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(run, "ignition.ign"), b, 0o644); err != nil {
			return err
		}
	}
	cmdline := "console=serial0,115200 rd.neednet=1 " + dhcpClass + ignition + bootplan.Plan{Disk: disk, SHA256: digest,
		OCI: base + "/oci", Layout: base + "/layout.json", Seed: base + "/seed.cpio", Report: base + "/stage0/report"}.Encode() + "\n"
	if err := os.WriteFile(filepath.Join(run, "cmdline.txt"), []byte(cmdline), 0o644); err != nil {
		return err
	}

	exe, err := a.Executable()
	if err != nil {
		return err
	}
	if err := a.CheckStatic(exe); err != nil {
		return err
	}
	files, err := ci.Files(spec.Installer.Initramfs)
	if err != nil {
		return fmt.Errorf("container %s: installer initramfs: %w", img.Container, err)
	}
	installer := files[spec.Installer.Initramfs]
	if err := writeFile(filepath.Join(run, initrdName), func(w io.Writer) error {
		// Padded: the kernel takes an appended archive at a 4-byte boundary.
		if _, err := w.Write(append(installer, make([]byte, (4-len(installer)%4)%4)...)); err != nil {
			return err
		}
		return initrd.BuildHook(w, exe)
	}); err != nil {
		return err
	}

	if err := writeSeed(run, img, cfg, fb, installed, bootCache); err != nil {
		return err
	}
	// The resolved layout, which the agent partitions by.
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(run, "layout.json"), b, 0o644); err != nil {
		return err
	}
	// The OCI layout, linked file by file (serve won't follow a link out).
	return filepath.WalkDir(layout, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(layout, p)
		dst := filepath.Join(run, "oci", rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		return linkOrCopy(p, dst)
	})
}

// linkOrCopy hard-links src to dst, copying when they're on different
// filesystems (serve won't follow a link out of its root).
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// maxCmdline bounds cmdline.txt. The kernel takes 2047 bytes, but on k52
// (Pi 5) only the first 864 bytes of a 1068-byte cmdline.txt reached the
// kernel (the firmware adds 265 of its own); where it is cut is unverified.
const maxCmdline = 850

// installedCmdline is the installed system's kernel command line: the
// image's, plus its hostname and root's SSH keys as systemd settings (the
// keys as a credential that systemd's provision.conf writes to
// /root/.ssh/authorized_keys). Base64 keeps the keys' spaces off the line.
func installedCmdline(base, hostname string, keys []string) (string, error) {
	line := strings.TrimSpace(base) + " systemd.hostname=" + hostname
	if len(keys) > 0 {
		line += " systemd.set_credential_binary=ssh.authorized_keys.root:" +
			base64.StdEncoding.EncodeToString([]byte(strings.Join(keys, "\n")+"\n"))
	}
	if len(line) > maxCmdline {
		return "", fmt.Errorf("the installed system's cmdline.txt would be %d bytes; a Pi passes on about %d (sshKeys: ed25519 keys are about 5 times smaller than RSA)", len(line), maxCmdline)
	}
	return line, nil
}

// writeFile writes path with fill.
func writeFile(path string, fill func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return errors.Join(fill(f), f.Close())
}

// Console events install waits for.
const (
	errorPrefix = "STAGE0-ERROR "
	readyEvent  = "ready"
)

// takeReport reports whether a report event counts: valid JSON, and not
// a console copy once an HTTP one has come (HTTP is the authority).
func takeReport(e event, haveHTTP bool) bool {
	return json.Valid([]byte(e.text)) && (e.http || !haveHTTP)
}

type event struct {
	kind, text string
	http       bool // from the agent's HTTP report, not the console
}

// watcher scans console output for the agent's lines and, once armed, the
// installed system's ready pattern.
type watcher struct {
	ctx    context.Context
	ready  *regexp.Regexp
	events chan event

	mu    sync.Mutex
	buf   []byte
	armed bool
}

func newWatcher(ctx context.Context, ready *regexp.Regexp) *watcher {
	return &watcher{ctx: ctx, ready: ready, events: make(chan event, 16)}
}

// send delivers an event unless the watcher is done.
func (w *watcher) send(e event) {
	select {
	case w.events <- e:
	case <-w.ctx.Done():
	}
}

// armReady starts looking for the ready pattern, in output from now on.
func (w *watcher) armReady() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed, w.buf = true, nil
}

func (w *watcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	var found []event
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		for _, k := range []string{agent.ReportPrefix, agent.InstalledPrefix, errorPrefix} {
			if j := strings.Index(line, k); j >= 0 {
				found = append(found, event{kind: k, text: line[j+len(k):]})
			}
		}
		if w.armed {
			if m := w.ready.FindString(line); m != "" {
				found = append(found, event{kind: readyEvent, text: m})
			}
		}
	}
	// A prompt has no newline after it.
	if w.armed {
		if m := w.ready.Find(w.buf); m != nil {
			found = append(found, event{kind: readyEvent, text: string(m)})
			w.buf = nil
		}
	}
	if len(w.buf) > 64<<10 {
		w.buf = w.buf[len(w.buf)-64<<10:]
	}
	w.mu.Unlock()
	for _, e := range found {
		w.send(e)
	}
	return len(p), nil
}

// firstBoot is how the installed system is configured on first boot
// (docs/firstboot.md): the node's choice, else the image config's.
type firstBoot struct {
	Mode      string // stage0, ignition or cloud-init
	CloudInit image.CloudInit
	Node      string
	Hostname  string
}

func resolveFirstBoot(node config.Node, img image.Config) firstBoot {
	fb := firstBoot{Mode: "stage0", Node: node.Name, Hostname: node.Hostname}
	switch {
	case node.FirstBoot != "":
		fb.Mode, fb.CloudInit = node.FirstBoot, node.CloudInit
	case img.FirstBoot != "":
		fb.Mode, fb.CloudInit = img.FirstBoot, img.CloudInit
	}
	return fb
}

// noCloud is the NoCloud seed's files: the user's user-data and
// network-config, and stage0's meta-data (a new instance per install, the
// node's hostname).
func (fb firstBoot) noCloud() map[string][]byte {
	files := map[string][]byte{
		"meta-data": fmt.Appendf(nil, "instance-id: %s-%d\nlocal-hostname: %s\n", fb.Node, time.Now().Unix(), fb.Hostname),
	}
	if fb.CloudInit.UserDataBytes != nil {
		files["user-data"] = fb.CloudInit.UserDataBytes
	}
	if fb.CloudInit.NetworkConfigBytes != nil {
		files["network-config"] = fb.CloudInit.NetworkConfigBytes
	}
	return files
}

// waitUp waits for the installed system: up is SSH answering at the best
// address known: in mode own its IPv4 lease from stage0, else (modes proxy
// and none, or no lease after leaseGrace: Ubuntu's eth0 config on a Pi 5)
// its IPv6 link-local address, found by its MAC. The console's ready
// pattern is progress only.
func (a *App) waitUp(ctx context.Context, res *installResult, node config.Node, n config.Network, leases *dhcpd.Server,
	w *watcher, start time.Time, say func(string, ...any)) error {
	if res.MAC == "" {
		return fmt.Errorf("%s: no MAC in the agent's report, so no way to find its system", node.Name)
	}
	hw, _ := net.ParseMAC(res.MAC)
	uctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	booted := time.Now()
	next := time.After(0) // look at once, then every upPoll
	last := "no address yet"
	for {
		select {
		case <-uctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s: installed, but its system isn't reachable after %s: %s (console: %s)", node.Name, readyWait, last, res.Log)
		case e := <-w.events:
			if e.kind == readyEvent && res.Ready == "" {
				res.Ready = e.text
				say("%s: %q on the console after %.0f s", node.Name, e.text, time.Since(start).Seconds())
			}
			continue
		case <-next:
			next = time.After(upPoll)
		}
		addr := ""
		if ip, ok := leases.Acked(hw); ok && n.Mode == "own" {
			addr, res.IPv4 = ip.String(), ip.String()
		} else if n.Mode == "own" && time.Since(booted) < leaseGrace {
			last = "no lease yet"
			continue // its lease is the address to give; link-local is the fallback
		} else {
			actx, acancel := context.WithTimeout(uctx, upPoll)
			a6, err := a.address(actx, *res, n)
			acancel()
			if err != nil {
				last = err.Error()
				continue
			}
			addr, res.Address = a6, a6
		}
		rctx, rcancel := context.WithTimeout(uctx, reachTry)
		r := a.Reach(rctx, addr)
		rcancel()
		if !r.OK() {
			last = fmt.Sprintf("%s: %s", addr, r.Err)
			continue
		}
		res.Reach = &r
		res.Seconds = time.Since(start).Round(time.Second).Seconds()
		hint := ""
		if addr == res.IPv4 {
			hint = fmt.Sprintf("; keep `stage0 serve %s` running to keep the address", n.Name)
		}
		say("%s: up after %.0f s: %s answers ping and ssh (%s)%s\n  ssh root@%s", node.Name, res.Seconds, addr, r.SSH, hint, addr)
		return nil
	}
}
