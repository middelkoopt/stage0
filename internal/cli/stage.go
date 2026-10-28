package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/middelkoopt/stage0/internal/bootplan"
	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/container"
	"github.com/middelkoopt/stage0/internal/dhcpd"
	"github.com/middelkoopt/stage0/internal/image"
	"github.com/middelkoopt/stage0/internal/initrd"
	"github.com/middelkoopt/stage0/internal/oci"
	"github.com/middelkoopt/stage0/internal/osconfig"
	"github.com/middelkoopt/stage0/internal/uki"
)

// initrdName is stage0's initramfs in the served boot files, named so it
// can't clash with the image's own.
const initrdName = "stage0-initrd"

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
		agent := func(w io.Writer) error { _, err := initrd.Build(w, exe, ""); return err }
		if distroInitrd == "" {
			return agent(w)
		}
		distro, err := os.Open(filepath.Join(run, distroInitrd))
		if err != nil {
			return fmt.Errorf("image %s: the agent needs its %s: %w", img.Name, distroInitrd, err)
		}
		defer distro.Close()
		return initrd.Append(w, distro, agent)
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
	// the agent appended, and the agent's command line.
	var rd bytes.Buffer
	f, err := os.Open(distroInitrd)
	if err != nil {
		return err
	}
	err = initrd.Append(&rd, f, func(w io.Writer) error { _, err := initrd.Build(w, exe, ""); return err })
	f.Close()
	if err != nil {
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
	case "stage0":
		// The agent writes the system's network, hostname and keys.
		tool := img.Network
		if v, ok := ci.Labels["pi.provision.network"]; ok && tool == "" {
			if err := json.Unmarshal([]byte(v), &tool); err != nil {
				return fmt.Errorf("container %s: label pi.provision.network: %w", img.Container, err)
			}
		}
		if tool != osconfig.Networkd && tool != osconfig.NetworkManager {
			return fmt.Errorf("container %s for %s: firstBoot stage0 needs its network tool: network: %s or %s in the image config, or the pi.provision.network label (got %q)",
				img.Container, node.Name, osconfig.Networkd, osconfig.NetworkManager, tool)
		}
		spec.System = &osconfig.System{Hostname: fb.Hostname, Keys: fb.Keys, Network: tool, Interfaces: fb.Interfaces}
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
		return initrd.Append(w, bytes.NewReader(installer), func(w io.Writer) error { return initrd.BuildHook(w, exe) })
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

// firstBoot is how the installed system is configured on first boot
// (docs/firstboot.md): the node's choice, else the image config's.
type firstBoot struct {
	Mode      string // stage0, ignition or cloud-init
	CloudInit image.CloudInit
	Node      string
	Hostname  string
	Keys      []string // root's SSH keys, from site.yaml
	// Interfaces are the node's, for a container in mode stage0.
	Interfaces []osconfig.Interface
}

func resolveFirstBoot(node config.Node, img image.Config) firstBoot {
	fb := firstBoot{Mode: "stage0", Node: node.Name, Hostname: node.Hostname, Interfaces: node.Interfaces}
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
