package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/middelkoopt/stage0/console"
	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/bootplan"
	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/image"
	"github.com/middelkoopt/stage0/internal/reach"
	"github.com/middelkoopt/stage0/internal/registry"
	"github.com/middelkoopt/stage0/internal/tftpd"
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
	p, err := a.planInstall(o)
	if err != nil {
		return err
	}
	node, n := p.node, p.n
	var poe *poeMachine
	if node.VM == nil {
		if poe, err = a.newPoEMachine(ctx, node, n, o.setPort, say); err != nil {
			return err
		}
	}
	progress := io.Discard // -o json prints only the result
	if a.output == "text" {
		progress = a.Stdout
	}
	run, err := a.stageFiles(ctx, p, o.cache, progress, say)
	if err != nil {
		return err
	}

	// Logs.
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return err
	}
	res := installResult{Node: node.Name, Image: p.img.Name, Disk: p.disk, Network: n.Name,
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
	w := newWatcher(cctx, p.ready)
	var consoles []io.Closer
	defer func() {
		for _, c := range consoles {
			c.Close()
		}
	}()
	follow := func(c io.ReadWriteCloser) {
		consoles = append(consoles, c)
		fmt.Fprintf(consoleLog, "\n=== stage0 install %s %s %s ===\n", node.Name, p.img.Name, time.Now().Format(time.RFC3339))
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
		vm, err := a.vmMachine(ctx, cctx, p, o.cache, slog, serveLog, progress)
		if err != nil {
			return err
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
	srv := &serving{done: make(chan struct{}), log: res.ServeLog}
	go func() {
		srv.err = a.serve(sctx, n, run, leases, files, report, slog)
		close(srv.done)
	}()
	defer func() { stopServe(); <-srv.done }()

	if err := m.bootAgent(ctx, follow); err != nil {
		return err
	}
	say("%s: powered on, booting stage0's agent", m.name())
	if err := waitInstalled(ctx, &res, m, w, srv, say); err != nil {
		return err
	}
	files.Close()
	markInstalled(&res, leases, o.ipv6)
	w.armReady()
	if err := m.bootDisk(ctx, follow); err != nil {
		return err
	}
	say("%s: rebooting from %s", node.Name, p.disk)
	if err := a.waitUp(ctx, &res, node, n, leases, w, start, say); err != nil {
		return err
	}
	if err := a.saveRecord(res); err != nil {
		return err
	}
	if err := a.findAddress(ctx, &res, n, say); err != nil {
		return err
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
		case <-srv.done:
			return srv.stopped()
		}
	}
	return nil
}

// installPlan is an install checked against the site and image configs.
type installPlan struct {
	site      *config.Site
	node      config.Node
	n         config.Network
	img       image.Config
	disk      string
	fb        firstBoot
	ready     *regexp.Regexp
	installed string // the installed system's cmdline.txt (Pis)
	server    string // stage0 in the agent's URLs: 10.0.3.1, or [fd00:10:3::1] with --ipv6
}

// planInstall checks everything before anything is downloaded or powered.
func (a *App) planInstall(o installOptions) (installPlan, error) {
	var p installPlan
	var err error
	if p.site, err = config.LoadSite(a.sitePath); err != nil {
		return p, err
	}
	if p.node, err = p.site.Node(o.node); err != nil {
		return p, err
	}
	node := p.node
	if p.disk = cmp.Or(o.disk, node.Disk); p.disk == "" {
		return p, fmt.Errorf("no disk for %s: give --disk or set disk: in %s", node.Name, a.sitePath)
	}
	if p.n, err = oneNetwork(p.site, cmp.Or(o.network, node.Network)); err != nil {
		return p, err
	}
	if p.img, err = image.Load(o.images, o.image); err != nil {
		return p, err
	}
	img := p.img
	switch {
	case node.VM != nil && !img.VM():
		return p, fmt.Errorf("%s is a VM, but image %s has no agent kernel for VMs", node.Name, img.Name)
	case node.VM == nil && img.VM():
		return p, fmt.Errorf("image %s is for VMs, but %s is not one", img.Name, node.Name)
	case o.ipv6 && !p.n.Server6.IsValid():
		return p, fmt.Errorf("--ipv6: network %q has no prefix6", p.n.Name)
	case node.VM != nil && img.Container != "":
		return p, errors.New("containers install on Pis only, for now")
	}
	p.fb = resolveFirstBoot(node, img)
	if p.fb.Mode == "ignition" && img.Container == "" {
		return p, fmt.Errorf("%s: firstBoot: ignition is for containers", node.Name)
	}
	p.ready, _ = regexp.Compile(img.Ready) // checked by Load
	if !img.VM() {
		if p.fb.Mode != "stage0" { // the chosen tool sets hostname and keys
			p.installed = strings.TrimSpace(img.Cmdline)
		} else if p.installed, err = installedCmdline(img.Cmdline, node.Hostname, p.site.SSHKeys); err != nil {
			return p, err
		}
	}
	// Over IPv6 the agent takes its address by SLAAC itself.
	p.server = p.n.Server.String()
	if o.ipv6 {
		p.server = "[" + p.n.Server6.String() + "]"
	}
	return p, nil
}

// stageFiles fetches the image and stages the node's boot files by the
// image's route, returning their directory.
func (a *App) stageFiles(ctx context.Context, p installPlan, cache string, progress io.Writer, say func(string, ...any)) (string, error) {
	secrets, err := config.LoadSecrets(a.secretsPath)
	if err != nil {
		return "", err
	}
	creds := map[string]registry.Credentials{}
	for host, r := range secrets.Registries {
		creds[host] = registry.Credentials{User: r.User, Password: r.Password}
	}
	img := p.img
	imgPath, err := image.FetchWith(ctx, img, cache, progress, creds)
	if err != nil {
		return "", err
	}
	say("stage0 %s; image %s: %s", version(), img.Name, imgPath)
	run := filepath.Join(cache, "run", p.node.Name)
	switch {
	case img.VM():
		base := "http://" + p.server
		cmdline := "console=ttyAMA0 " + bootplan.Plan{Disk: p.disk, SHA256: img.SHA256,
			URL: base + "/images/" + img.File(), Report: base + "/stage0/report"}.Encode()
		err = a.stageVM(ctx, run, img, imgPath, p.node, p.site.SSHKeys, p.fb, cmdline, cache, progress)
	case img.Container != "":
		err = a.stageContainer(run, imgPath, img, p.node, p.fb, p.installed, p.disk, p.server, filepath.Join(cache, "boot"))
	default:
		err = a.stage(run, imgPath, img, p.fb, p.installed, p.disk, p.server, filepath.Join(cache, "boot", img.SHA256))
	}
	if err != nil {
		return "", err
	}
	say("staged boot files in %s", run)
	return run, nil
}

// vmMachine starts the test bed's switch for the node's network (it runs
// until cctx is done) and returns the node's VM, firmware fetched.
func (a *App) vmMachine(ctx, cctx context.Context, p installPlan, cache string, log *slog.Logger, serveLog io.Writer, progress io.Writer) (*vmMachine, error) {
	node, n := p.node, p.n
	vmDir := filepath.Join(cache, "vm")
	sock := filepath.Join(vmDir, n.Name+".sock")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return nil, err
	}
	if err := a.runSwitch(cctx, sock, n.Interface, log); err != nil {
		return nil, err
	}
	firmware := node.VM.Firmware
	if firmware == "" {
		var err error
		if firmware, err = image.FetchDebFile(ctx, uefiFirmware, uefiFirmwareCode, filepath.Join(cache, "firmware"), progress); err != nil {
			return nil, err
		}
		if _, err = image.FetchDebFile(ctx, uefiFirmware, uefiFirmwareVars, filepath.Join(cache, "firmware"), progress); err != nil {
			return nil, err
		}
	}
	vm, err := newVMMachine(a, node, firmware, vmDir, sock, serveLog)
	if err != nil {
		return nil, err
	}
	if p.img.NoCloud {
		vm.serial = "ds=nocloud;s=http://" + n.Server.String() + "/nocloud/" + node.Name + "/"
	}
	return vm, nil
}

// serving is serve running for an install.
type serving struct {
	done chan struct{} // closed when serve returns
	err  error         // what it returned
	log  string
}

func (s *serving) stopped() error { return fmt.Errorf("serve stopped: %v (log: %s)", s.err, s.log) }

// waitInstalled waits for the agent's STAGE0-INSTALLED, keeping its report
// and result in res; on the agent's error it powers the node off.
func waitInstalled(ctx context.Context, res *installResult, m machine, w *watcher, srv *serving, say func(string, ...any)) error {
	actx, cancel := context.WithTimeout(ctx, agentWait)
	defer cancel()
	reportByHTTP := false
	for {
		select {
		case <-actx.Done():
			return fmt.Errorf("%s: the agent did not report STAGE0-INSTALLED (logs: %s, %s): %w", res.Node, res.Log, res.ServeLog, actx.Err())
		case <-srv.done:
			return srv.stopped()
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
					say("%s: agent running", res.Node)
				}
				res.Report, reportByHTTP = json.RawMessage(e.text), reportByHTTP || e.http
			case agent.InstalledPrefix:
				if !json.Valid([]byte(e.text)) {
					continue // the other copy follows
				}
				res.Installed = json.RawMessage(e.text)
				var r agent.Result
				json.Unmarshal(res.Installed, &r)
				say("%s: wrote %.1f GB to %s in %.0f s", res.Node, float64(r.Bytes)/1e9, r.Disk, r.Seconds)
				return nil
			case errorPrefix:
				if err := m.off(ctx); err != nil {
					say("%s: powering off: %v", res.Node, err)
				}
				return fmt.Errorf("%s: agent: %s (node powered off; logs: %s, %s)", res.Node, e.text, res.Log, res.ServeLog)
			}
		}
	}
}

// markInstalled records the node as installed in leases, by the MAC it
// netbooted on: from now on its PXE requests get no answer, so it boots
// from its disk, and its installed system's DHCP gets the same address.
func markInstalled(res *installResult, leases *dhcpd.Server, ipv6 bool) {
	macs, err := res.macs()
	if err != nil {
		return
	}
	for _, m := range macs {
		ip, ok := leases.Install(m)
		if !ok && ipv6 { // it netbooted over IPv6: no IPv4 lease yet
			ip, ok = leases.Reserve(m)
		}
		if ok {
			res.MAC = m.String()
			if ip.IsValid() { // none in proxy mode: the other server leases
				res.IPv4 = ip.String()
			}
			return
		}
	}
}

// findAddress finds the installed system's IPv6 address, unless waitUp
// already did, and records it.
func (a *App) findAddress(ctx context.Context, res *installResult, n config.Network, say func(string, ...any)) error {
	if res.Address != "" {
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, addressWait)
	defer cancel()
	addr, err := a.address(fctx, *res, n)
	if err != nil {
		say("%s: no IPv6 address found yet (%v); try: stage0 get address %s", res.Node, err, res.Node)
		return nil
	}
	res.Address = addr
	say("%s: %s\n  ssh root@%s", res.Node, addr, addr)
	return a.saveRecord(*res) // now with the address
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
