package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/console"
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/image"
	"github.com/middelkoopt/stage0/internal/initrd"
	"github.com/middelkoopt/stage0/internal/reach"
)

// Time limits for the waits, unless --timeout is given for the whole.
const (
	reachWait = time.Minute      // lease to answering ping and SSH
	agentWait = 20 * time.Minute // power on to STAGE0-INSTALLED
)

// readyWait is the time from the power cycle to the installed system's
// prompt (a variable so tests can shorten it).
var readyWait = 10 * time.Minute

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
	case o.ipv6 && node.VM != nil:
		return errors.New("--ipv6: not for VMs yet")
	}
	ready, _ := regexp.Compile(img.Ready) // checked by Load
	var installed string
	if !img.VM() {
		if installed, err = installedCmdline(img.Cmdline, node.Hostname, site.SSHKeys); err != nil {
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
	imgPath, err := image.Fetch(ctx, img, o.cache, progress)
	if err != nil {
		return err
	}
	say("image %s: %s", img.Name, imgPath)
	run := filepath.Join(o.cache, "run", node.Name)
	var kernel, initrdPath string
	if img.VM() {
		kernel, initrdPath, err = a.stageVM(ctx, run, img, imgPath, node, site.SSHKeys, filepath.Join(o.cache, "agent", img.Name), progress)
	} else {
		server := n.Server.String()
		if o.ipv6 {
			server = "[" + n.Server6.String() + "]"
		}
		err = a.stage(run, imgPath, img, installed, node.Hostname, disk, server, filepath.Join(o.cache, "boot", img.SHA256))
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
		go console.Capture(cctx, c, io.MultiWriter(consoleLog, w), nil)
	}
	hasConsole := node.Console != nil || node.VM != nil
	if node.Console != nil {
		portIO, err := a.OpenConsole(node.Console.Device, node.Console.Baud)
		if err != nil {
			return err
		}
		follow(portIO)
	}
	report := func(from, kind string, body []byte) {
		w.send(event{map[string]string{"report": agent.ReportPrefix, "installed": agent.InstalledPrefix, "error": errorPrefix}[kind], string(body)})
	}

	// Serve for the whole install: the agent, then the installed system's
	// DHCP. Other installed nodes keep their addresses.
	others, err := a.installedOn(n, node.Name)
	if err != nil {
		return err
	}
	slog := slog.New(slog.NewTextHandler(serveLog, nil)).With("network", n.Name)
	leases := a.newLeases(n, others, slog)

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
		vm, err := newVMMachine(a, node, vmDir, sock, serveLog)
		if err != nil {
			return err
		}
		base := "http://" + n.Server.String()
		vm.kernel, vm.initrd = kernel, initrdPath
		vm.cmdline = fmt.Sprintf("console=ttyAMA0 stage0.disk=%s stage0.sha256=%s stage0.url=%s/images/%s stage0.report=%s/stage0/report",
			disk, img.SHA256, base, img.File(), base)
		if img.NoCloud {
			vm.serial = "ds=nocloud;s=" + base + "/nocloud/" + node.Name + "/"
		}
		m = vm
		defer vm.off(ctx)
	}
	res.Port = m.port()
	sctx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = a.serve(sctx, n, run, leases, report, slog)
		close(serveDone)
	}()
	defer func() { stopServe(); <-serveDone }()

	if err := m.bootAgent(ctx, follow); err != nil {
		return err
	}
	say("%s: powered on, booting stage0's agent", m.name())

	actx, cancel := context.WithTimeout(ctx, agentWait)
	defer cancel()
	for done := false; !done; {
		select {
		case <-actx.Done():
			return fmt.Errorf("%s: the agent did not report STAGE0-INSTALLED (logs: %s, %s): %w", node.Name, res.Log, res.ServeLog, actx.Err())
		case <-serveDone:
			return fmt.Errorf("serve stopped: %v (log: %s)", serveErr, res.ServeLog)
		case e := <-w.events:
			switch e.kind {
			case agent.ReportPrefix:
				if res.Report == nil { // it may come by console and by HTTP
					say("%s: agent running", node.Name)
				}
				res.Report = json.RawMessage(e.text)
			case agent.InstalledPrefix:
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

	// From now on its PXE requests get no answer, so it boots from its
	// disk; its installed system's DHCP gets the same address.
	if macs, err := res.macs(); err == nil {
		for _, m := range macs {
			ip, ok := leases.Install(m)
			if !ok && o.ipv6 { // it netbooted over IPv6: no IPv4 lease yet
				ip, ok = leases.Reserve(m)
			}
			if ok {
				res.MAC, res.IPv4 = m.String(), ip.String()
				break
			}
		}
	}
	w.armReady()
	if err := m.bootDisk(ctx, follow); err != nil {
		return err
	}
	say("%s: rebooting from %s", node.Name, disk)
	rctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
waitReady:
	for hasConsole && res.Ready == "" {
		select {
		case <-rctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A quiet console isn't a failed install: the lease and ping
			// and SSH decide, as for a node without a console.
			say("%s: no %q on the console (console: %s); checking the network instead", node.Name, img.Ready, res.Log)
			break waitReady
		case e := <-w.events:
			if e.kind == readyEvent {
				res.Ready = e.text
			}
		}
	}
	quiet := !hasConsole || res.Ready == "" // up only if the network says so
	res.Seconds = time.Since(start).Round(time.Second).Seconds()
	if !quiet {
		say("%s: up (%q on the console) after %.0f s", node.Name, res.Ready, res.Seconds)
	}
	// Without a console, the lease and ping and SSH are how it shows it's up.
	leaseWait := addressWait
	if quiet {
		leaseWait = readyWait
	}
	if res.MAC == "" && quiet {
		return fmt.Errorf("%s: no MAC with a lease in the agent's report, so no way to tell it's up", node.Name)
	}
	if res.MAC != "" {
		hw, _ := net.ParseMAC(res.MAC)
		if waitAcked(ctx, leases, hw, leaseWait) {
			// The goal: it answers ping and SSH.
			rctx, cancel := context.WithTimeout(ctx, reachWait)
			r := a.Reach(rctx, res.IPv4)
			cancel()
			res.Reach = &r
			if r.OK() {
				say("%s: %s answers ping and ssh (%s); keep `stage0 serve %s` running to keep the address\n  ssh root@%s",
					node.Name, res.IPv4, r.SSH, n.Name, res.IPv4)
			} else if quiet {
				return fmt.Errorf("%s: installed and leased %s, but not reachable: %s", node.Name, res.IPv4, r.Err)
			} else {
				say("%s: %s is leased but not reachable yet: %s", node.Name, res.IPv4, r.Err)
			}
		} else if quiet {
			return fmt.Errorf("%s: installed, but its system never asked stage0 for its address %s", node.Name, res.IPv4)
		} else {
			say("%s: its system has not asked stage0 for its address %s yet", node.Name, res.IPv4)
		}
	}
	if err := a.saveRecord(res); err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, addressWait)
	defer cancel()
	if addr, err := a.address(fctx, res, n); err != nil {
		say("%s: no IPv6 address found yet (%v); try: stage0 get address %s", node.Name, err, node.Name)
	} else {
		res.Address = addr
		say("%s: %s\n  ssh root@%s", node.Name, addr, addr)
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

// waitAcked waits until leases has acknowledged mac's address.
func waitAcked(ctx context.Context, leases *dhcpd.Server, mac net.HardwareAddr, limit time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		if _, ok := leases.Acked(mac); ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
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
func (a *App) stage(run, imgPath string, img image.Config, installed, hostname, disk, server, bootCache string) error {
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
	cmdline := fmt.Sprintf("console=serial0,115200 %sstage0.disk=%s stage0.sha256=%s stage0.url=%s/images/%s stage0.seed=%s/seed.cpio stage0.report=%s/stage0/report\n",
		netcfg, disk, img.SHA256, base, img.File(), base, base)
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
			if _, err := io.Copy(w, distro); err != nil {
				return err
			}
		}
		_, err := initrd.Build(w, exe, "")
		return err
	}); err != nil {
		return err
	}

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
	// cloud-init images (user-data on the boot partition) would otherwise
	// set their own hostname at first boot.
	if ud, err := os.ReadFile(filepath.Join(bootCache, "user-data")); err == nil {
		if err := os.WriteFile(filepath.Join(seed, "user-data"), image.SetCloudInitHostname(ud, hostname), 0o644); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(run, "seed.cpio"), func(w io.Writer) error {
		_, err := initrd.BuildSeed(w, seed)
		return err
	}); err != nil {
		return err
	}
	if err := os.RemoveAll(seed); err != nil {
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
	agentCache string, progress io.Writer) (kernel, agentInitrd string, err error) {
	if kernel, err = image.FetchFile(ctx, *img.Kernel, agentCache, progress); err != nil {
		return "", "", err
	}
	distroInitrd, err := image.FetchFile(ctx, *img.Initrd, agentCache, progress)
	if err != nil {
		return "", "", err
	}
	if err := os.RemoveAll(run); err != nil {
		return "", "", err
	}
	images := filepath.Join(run, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return "", "", err
	}
	if err := linkOrCopy(imgPath, filepath.Join(images, img.File())); err != nil {
		return "", "", err
	}
	nc := filepath.Join(run, "nocloud", node.Name)
	if err := os.MkdirAll(nc, 0o755); err != nil {
		return "", "", err
	}
	meta := fmt.Sprintf("instance-id: %s-%d\nlocal-hostname: %s\n", node.Name, time.Now().Unix(), node.Hostname)
	if err := errors.Join(os.WriteFile(filepath.Join(nc, "meta-data"), []byte(meta), 0o644),
		os.WriteFile(filepath.Join(nc, "user-data"), []byte(nocloudUserData(node.Hostname, keys)), 0o644)); err != nil {
		return "", "", err
	}

	exe, err := a.Executable()
	if err != nil {
		return "", "", err
	}
	if err := a.CheckStatic(exe); err != nil {
		return "", "", err
	}
	agentInitrd = filepath.Join(run, "initrd")
	err = writeFile(agentInitrd, func(w io.Writer) error {
		f, err := os.Open(distroInitrd)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		_, err = initrd.Build(w, exe, "")
		return err
	})
	return kernel, agentInitrd, err
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

type event struct{ kind, text string }

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
				found = append(found, event{k, line[j+len(k):]})
			}
		}
		if w.armed {
			if m := w.ready.FindString(line); m != "" {
				found = append(found, event{readyEvent, m})
			}
		}
	}
	// A prompt has no newline after it.
	if w.armed {
		if m := w.ready.Find(w.buf); m != nil {
			found = append(found, event{readyEvent, string(m)})
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
