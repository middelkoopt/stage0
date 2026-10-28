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

	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/bootplan"
	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/console"
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
