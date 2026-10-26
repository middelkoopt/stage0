package cli

import (
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
	"github.com/middelkoopt/stage0/internal/netswitch"
	"github.com/middelkoopt/stage0/internal/reach"
)

// Time limits for the waits, unless --timeout is given for the whole.
const (
	reachWait = time.Minute      // lease to answering ping and SSH
	agentWait = 20 * time.Minute // power on to STAGE0-INSTALLED
	readyWait = 10 * time.Minute // power cycle to the installed system's prompt
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
	var setPort bool
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
				setPort: setPort,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&imageName, "image", "", "image to install: images/NAME.yaml")
	f.StringVar(&imagesDir, "images", "images", "directory of image config files")
	f.StringVar(&cacheDir, "cache", "", "where images and staged boot files go (default: the user cache dir/stage0)")
	f.StringVar(&disk, "disk", "", "disk to write, e.g. mmcblk0 (default: the node's disk in site.yaml)")
	f.StringVar(&network, "network", "", "network to install over (default: the only one in site.yaml)")
	f.BoolVar(&setPort, "set-port", false, "move the node's port to the network's VLAN first (its old PVID stays tagged)")
	return cmd
}

type installOptions struct {
	node, image, images, cache, disk, network string
	setPort                                   bool
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
	n, err := oneNetwork(site, o.network)
	if err != nil {
		return err
	}
	img, err := image.Load(o.images, o.image)
	if err != nil {
		return err
	}
	ready, _ := regexp.Compile(img.Ready) // checked by Load
	installed, err := installedCmdline(img.Cmdline, node.Hostname, site.SSHKeys)
	if err != nil {
		return err
	}

	sw, err := a.openSite()
	if err != nil {
		return err
	}
	st, err := sw.Status(ctx)
	if err != nil {
		return err
	}
	port, err := st.Find(node.Name)
	if err != nil {
		return err
	}
	if port.PoE == nil {
		return fmt.Errorf("%s has no PoE, so stage0 can't power it", label(port))
	}
	if n.VLAN != "" {
		v, err := st.FindVLAN(n.VLAN)
		if err != nil {
			return fmt.Errorf("network %q: %w", n.Name, err)
		}
		if port.PVID != v.ID && !o.setPort {
			return fmt.Errorf("%s has PVID %d, not %s(%d); move it with --set-port or: stage0 set port %s --pvid %s",
				label(port), port.PVID, v.Name, v.ID, node.Name, v.Name)
		}
		if port.PVID != v.ID {
			// The old PVID stays, tagged, so nothing the port carried is lost.
			tagged := slices.DeleteFunc(append(slices.Clone(port.Tagged), port.PVID), func(id int) bool { return id == v.ID })
			slices.Sort(tagged)
			ch, err := sw.SetVLANs(ctx, port.ID, v.ID, tagged)
			if err != nil {
				return err
			}
			say("%s: %s -> %s", label(port), showVLANs(st, ch.Before), showVLANs(st, ch.After))
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
	if err := a.stage(run, imgPath, img, installed, node.Hostname, disk, n.Server.String(), filepath.Join(o.cache, "boot", img.SHA256)); err != nil {
		return err
	}
	say("staged boot files in %s", run)

	// Logs.
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return err
	}
	res := installResult{Node: node.Name, Port: port.ID, Image: img.Name, Disk: disk, Network: n.Name,
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
	if node.Console != nil {
		portIO, err := a.OpenConsole(node.Console.Device, node.Console.Baud)
		if err != nil {
			return err
		}
		defer portIO.Close()
		fmt.Fprintf(consoleLog, "\n=== stage0 install %s %s %s ===\n", node.Name, img.Name, time.Now().Format(time.RFC3339))
		go console.Capture(cctx, portIO, io.MultiWriter(consoleLog, w), nil)
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
	sctx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = a.serve(sctx, n, run, leases, report, slog)
		close(serveDone)
	}()
	defer func() { stopServe(); <-serveDone }()

	if err := a.powerCycle(ctx, sw, port.ID); err != nil {
		return err
	}
	say("%s: powered on, netbooting stage0's agent", label(port))

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
				if _, err := sw.SetPoE(ctx, port.ID, false); err != nil {
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
			if ip, ok := leases.Install(m); ok {
				res.MAC, res.IPv4 = m.String(), ip.String()
				break
			}
		}
	}
	w.armReady()
	if err := a.powerCycle(ctx, sw, port.ID); err != nil {
		return err
	}
	say("%s: rebooting from %s", node.Name, disk)
	rctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	for node.Console != nil && res.Ready == "" {
		select {
		case <-rctx.Done():
			return fmt.Errorf("%s: installed, but no %q on the console (console: %s): %w", node.Name, img.Ready, res.Log, rctx.Err())
		case e := <-w.events:
			if e.kind == readyEvent {
				res.Ready = e.text
			}
		}
	}
	res.Seconds = time.Since(start).Round(time.Second).Seconds()
	if node.Console != nil {
		say("%s: up (%q on the console) after %.0f s", node.Name, res.Ready, res.Seconds)
	}
	// Without a console, the lease and ping and SSH are how it shows it's up.
	leaseWait := addressWait
	if node.Console == nil {
		leaseWait = readyWait
	}
	if res.MAC == "" && node.Console == nil {
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
			} else if node.Console == nil {
				return fmt.Errorf("%s: installed and leased %s, but not reachable: %s", node.Name, res.IPv4, r.Err)
			} else {
				say("%s: %s is leased but not reachable yet: %s", node.Name, res.IPv4, r.Err)
			}
		} else if node.Console == nil {
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
		return a.writeJSON(res)
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

// powerCycle powers a port off (if it isn't already) and on.
func (a *App) powerCycle(ctx context.Context, sw netswitch.Controller, port int) error {
	if _, err := sw.SetPoE(ctx, port, false); err != nil {
		return err
	}
	_, err := sw.SetPoE(ctx, port, true)
	return err
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
	// (enable_uart: a Pi 4's GPIO serial port is off without it).
	agentCfg := image.EnsureConfig(cfg, append(slices.Clone(img.Config), "initramfs "+initrdName+" followkernel", "enable_uart=1"))
	if err := os.WriteFile(filepath.Join(run, "config.txt"), agentCfg, 0o644); err != nil {
		return err
	}
	base := "http://" + server
	// serial0 is the Pi's console UART.
	cmdline := fmt.Sprintf("console=serial0,115200 ip=dhcp stage0.disk=%s stage0.sha256=%s stage0.url=%s/images/%s stage0.seed=%s/seed.cpio stage0.report=%s/stage0/report\n",
		disk, img.SHA256, base, img.File(), base, base)
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
	if err := writeFile(filepath.Join(run, initrdName), func(w io.Writer) error {
		_, err := initrd.Build(w, exe, "")
		return err
	}); err != nil {
		return err
	}

	seed := filepath.Join(run, ".seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(seed, "cmdline.txt"), []byte(installed+"\n"), 0o644); err != nil {
		return err
	}
	if len(img.Config) > 0 {
		if err := os.WriteFile(filepath.Join(seed, "config.txt"), image.EnsureConfig(cfg, img.Config), 0o644); err != nil {
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
