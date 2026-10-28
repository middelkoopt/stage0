package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/middelkoopt/stage0/internal/container"
	"github.com/middelkoopt/stage0/internal/oci"
)

// installContainer installs the container image served at plan.OCI
// (docs/containers.md): partitions the disk as plan.Layout says (stage0
// resolves it from the node, image config and labels) with the
// installer's own tools (sgdisk, mkfs), unpacks its layers onto the root,
// copies what belongs on the other partitions (the Pi's boot files) onto
// them, and writes fstab, hosts and resolv.conf. Every blob is checked
// against its digest, from the manifest's (plan.SHA256) down.
func installContainer(out io.Writer, plan *Plan) (Result, error) {
	setClock(out, plan.OCI+"/index.json")
	start := time.Now()
	dev := "/dev/" + plan.Disk
	res := Result{Disk: dev, Checksum: plan.SHA256}
	img, err := oci.OpenURL(plan.OCI, "sha256:"+plan.SHA256)
	if err != nil {
		return res, err
	}
	spec, err := fetchLayout(plan.Layout)
	if err != nil {
		return res, err
	}
	parts := make([]string, len(spec.Partitions))
	if spec.Ignition {
		// The installer's Ignition partitioned and formatted the disk
		// before this hook ran: take its partitions by label.
		fmt.Fprintf(out, "stage0 agent: %s partitioned by Ignition\n", dev)
		for i, p := range spec.Partitions {
			parts[i] = "/dev/disk/by-partlabel/" + p.Name
			if err := waitDev(parts[i]); err != nil {
				return res, err
			}
		}
	} else {
		fmt.Fprintf(out, "stage0 agent: partitioning %s\n", dev)
		// Zap first, on its own: given an old MBR table (k52 after Ubuntu),
		// sgdisk complains about it and exits 2 even when it goes on to
		// write the new one. Then the new table must succeed.
		if err := runTool(out, "sgdisk", "--zap-all", dev); err != nil {
			fmt.Fprintf(out, "stage0 agent: zapping the old table: %v (going on)\n", err)
		}
		if err := runTool(out, "sgdisk", sgdiskArgs(spec, dev)...); err != nil {
			return res, err
		}
		for i, p := range spec.Partitions {
			parts[i] = "/dev/" + partition(plan.Disk, i+1)
			if err := waitDev(parts[i]); err != nil {
				return res, err
			}
			cmd, args := mkfsArgs(p, parts[i])
			if err := runTool(out, cmd, args...); err != nil {
				return res, err
			}
		}
	}

	const mnt = "/mnt/root"
	last := len(spec.Partitions) - 1
	if err := mount(parts[last], mnt, "ext4"); err != nil {
		return res, err
	}
	defer syscall.Unmount(mnt, 0)
	// ext4 partitions (a separate /var) are mounted in the tree, so the
	// layers land on them with owners, links and modes; FAT ones (the
	// boot partition), which can't keep those, are filled afterwards.
	mounted := []string{}
	unmountAll := func() error {
		var errs []error
		for _, d := range slices.Backward(mounted) {
			if err := syscall.Unmount(d, 0); err != nil {
				errs = append(errs, fmt.Errorf("unmounting %s: %w", d, err))
			}
		}
		mounted = nil
		return errors.Join(errs...)
	}
	defer unmountAll()
	for _, i := range mountOrder(spec) {
		d := filepath.Join(mnt, spec.Partitions[i].Path)
		if err := mount(parts[i], d, "ext4"); err != nil {
			return res, err
		}
		mounted = append(mounted, d)
	}
	fmt.Fprintf(out, "stage0 agent: unpacking %s onto %s\n", plan.OCI, parts[last])
	if res.Bytes, err = img.Unpack(mnt); err != nil {
		return res, err
	}
	for i, p := range spec.Partitions[:last] {
		if p.Type != "vfat" {
			continue
		}
		if err := movePartition(filepath.Join(mnt, p.Path), parts[i], p.Type); err != nil {
			return res, fmt.Errorf("partition %s: %w", p.Name, err)
		}
	}
	if err := setupRoot(mnt, spec, img.Labels); err != nil {
		return res, err
	}

	// A Pi has no clock battery: systemd moves its clock at boot to at
	// least this file's time, which the agent's clock (set from stage0's
	// server) makes the install time.
	if err := touchClockEpoch(mnt, time.Now()); err != nil {
		return res, err
	}
	switch spec.FirstBoot {
	case "cloud-init":
		fmt.Fprintln(out, "stage0 agent: writing the cloud-init seed")
		if err := writeNoCloud(mnt, spec.CloudInit); err != nil {
			return res, err
		}
	case "ignition":
		if err := ignitionFiles(out, mnt, spec, parts); err != nil {
			return res, err
		}
	}
	// Layers built without SELinux carry no labels, and an enforcing
	// system with unlabelled files can't even start systemd (k52; a
	// first-boot /.autorelabel never gets to run): label everything now,
	// with the system's own policy.
	if typ, ok := selinuxType(mnt); ok && selinuxOn(mnt) {
		fmt.Fprintln(out, "stage0 agent: SELinux: labelling the installed files")
		fc := filepath.Join(mnt, "etc", "selinux", typ, "contexts", "files", "file_contexts")
		// Each ext4 filesystem by name: setfiles left a /var mounted inside
		// the root unlabelled (k52: NetworkManager then failed).
		args := []string{"-F", "-r", mnt, fc, mnt}
		for _, i := range mountOrder(spec) {
			args = append(args, filepath.Join(mnt, spec.Partitions[i].Path))
		}
		if err := runTool(io.Discard, "setfiles", args...); err != nil {
			return res, fmt.Errorf("SELinux labelling (the installer needs setfiles): %w", err)
		}
	}
	syscall.Sync()
	if err := unmountAll(); err != nil {
		return res, err
	}
	if err := syscall.Unmount(mnt, 0); err != nil {
		return res, fmt.Errorf("unmounting %s: %w", mnt, err)
	}
	res.Seconds = time.Since(start).Seconds()
	fmt.Fprintf(out, "stage0 agent: container installed in %.0f s\n", res.Seconds)
	return res, nil
}

// mountOrder lists the partitions to mount in the root's tree before
// unpacking (ext4, not the root), parents before children.
func mountOrder(spec container.Spec) []int {
	var idx []int
	for i, p := range spec.Partitions[:len(spec.Partitions)-1] {
		if p.Type == "ext4" {
			idx = append(idx, i)
		}
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		return strings.Count(spec.Partitions[a].Path, "/") - strings.Count(spec.Partitions[b].Path, "/")
	})
	return idx
}

// sgdiskArgs makes a fresh GPT with the spec's partitions, in order, the
// last filling the disk.
func sgdiskArgs(spec container.Spec, dev string) []string {
	args := []string{"--clear"}
	for i, p := range spec.Partitions {
		n := i + 1
		size := "0"
		if p.Bytes > 0 {
			size = fmt.Sprintf("+%dK", p.Bytes>>10)
		}
		code := "8300" // Linux filesystem
		if p.Type == "vfat" {
			code = "0700" // Microsoft basic data, which the Pi firmware reads
		}
		args = append(args, fmt.Sprintf("--new=%d:0:%s", n, size), fmt.Sprintf("--typecode=%d:%s", n, code),
			fmt.Sprintf("--change-name=%d:%s", n, p.Name))
	}
	return append(args, dev)
}

// mkfsArgs formats a partition, labelled with its name (FAT labels are
// upper case, at most 11 characters).
func mkfsArgs(p container.Partition, dev string) (string, []string) {
	if p.Type == "vfat" {
		label := strings.ToUpper(p.Name)
		return "mkfs.vfat", []string{"-F", "32", "-n", label[:min(len(label), 11)], dev}
	}
	return "mkfs.ext4", []string{"-F", "-q", "-L", p.Name, dev}
}

// fstab mounts the partitions by GPT name, the root first.
func fstab(spec container.Spec) string {
	last := len(spec.Partitions) - 1
	line := func(p container.Partition, pass int) string {
		return fmt.Sprintf("PARTLABEL=%s %s %s defaults 0 %d\n", p.Name, p.Path, p.Type, pass)
	}
	s := line(spec.Partitions[last], 1)
	for _, p := range spec.Partitions[:last] {
		s += line(p, 2)
	}
	return s
}

// setupRoot writes what a container leaves to its runtime: /etc/fstab,
// /etc/hosts (pi.provision.hosts, a JSON list of lines) and
// /etc/resolv.conf (pi.provision.resolv, a JSON string: a symlink target).
func setupRoot(root string, spec container.Spec, labels map[string]string) error {
	etc := filepath.Join(root, "etc")
	if err := os.WriteFile(filepath.Join(etc, "fstab"), []byte(fstab(spec)), 0o644); err != nil {
		return err
	}
	if v, ok := labels["pi.provision.hosts"]; ok {
		var lines []string
		if err := json.Unmarshal([]byte(v), &lines); err != nil {
			return fmt.Errorf("pi.provision.hosts: %w", err)
		}
		if err := os.WriteFile(filepath.Join(etc, "hosts"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return err
		}
	}
	if v, ok := labels["pi.provision.resolv"]; ok {
		var target string
		if err := json.Unmarshal([]byte(v), &target); err != nil {
			return fmt.Errorf("pi.provision.resolv: %w", err)
		}
		p := filepath.Join(etc, "resolv.conf")
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Symlink(target, p); err != nil {
			return err
		}
	}
	return nil
}

// movePartition copies the tree at dir onto the partition dev, then
// empties dir, which stays as its mount point.
func movePartition(dir, dev, fstype string) error {
	const mnt = "/mnt/part"
	if err := mount(dev, mnt, fstype); err != nil {
		return err
	}
	err := os.CopyFS(mnt, os.DirFS(dir))
	syscall.Sync()
	if uerr := syscall.Unmount(mnt, 0); err == nil && uerr != nil {
		err = fmt.Errorf("unmounting %s: %w", dev, uerr)
	}
	if err != nil {
		return err
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := os.RemoveAll(filepath.Join(dir, n.Name())); err != nil {
			return err
		}
	}
	return nil
}

func mount(dev, dir, fstype string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(dev, dir, fstype, 0, ""); err != nil {
		return fmt.Errorf("mounting %s on %s: %w", dev, dir, err)
	}
	return nil
}

// runTool runs one of the installer's tools, its output to out.
func runTool(out io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// touchClockEpoch sets the time of root's /usr/lib/clock-epoch (made if
// missing): systemd, as PID 1, advances a clock that is behind it.
func touchClockEpoch(root string, at time.Time) error {
	p := filepath.Join(root, "usr", "lib", "clock-epoch")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(p, at, at)
}

// serverTime is the time stage0's HTTP server gives (its Date header) for
// url.
func serverTime(url string) (time.Time, error) {
	resp, err := http.Head(url)
	if err != nil {
		return time.Time{}, err
	}
	resp.Body.Close()
	return http.ParseTime(resp.Header.Get("Date"))
}

// setClock sets the node's clock from stage0's server when it is more
// than a minute off (a Pi starts at its firmware's idea of the time), so
// the files the agent writes, and the installed system, start near now.
func setClock(out io.Writer, url string) {
	t, err := serverTime(url)
	if err != nil {
		fmt.Fprintf(out, "stage0 agent: clock not set: %v\n", err)
		return
	}
	if time.Since(t).Abs() < time.Minute {
		return
	}
	tv := syscall.NsecToTimeval(t.UnixNano())
	if err := syscall.Settimeofday(&tv); err != nil {
		fmt.Fprintf(out, "stage0 agent: clock not set: %v\n", err)
		return
	}
	fmt.Fprintf(out, "stage0 agent: clock set to %s from %s\n", t.UTC().Format(time.RFC3339), url)
}

// fetchLayout gets the resolved layout stage0 serves.
func fetchLayout(url string) (container.Spec, error) {
	var s container.Spec
	resp, err := http.Get(url)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("%s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %w", url, err)
	}
	if err := s.Validate(); err != nil {
		return s, fmt.Errorf("%s: %w", url, err)
	}
	return s, nil
}

// writeNoCloud writes cloud-init's NoCloud seed into the installed root
// (/var/lib/cloud/seed/nocloud, which cloud-init always looks at), for
// root only: user-data may hold secrets.
func writeNoCloud(root string, ci *container.CloudInit) error {
	if ci == nil {
		return errors.New("firstBoot cloud-init without its data")
	}
	dir := filepath.Join(root, "var", "lib", "cloud", "seed", "nocloud")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, body := range map[string]string{"user-data": ci.UserData, "meta-data": ci.MetaData, "network-config": ci.NetworkConfig} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ignitionFiles has the installer's Ignition apply its config's files,
// users and units to the installed system at root: its files stage, with
// every partition mounted where it belongs (the FAT ones too), from the
// config Ignition fetched at boot.
func ignitionFiles(out io.Writer, root string, spec container.Spec, parts []string) error {
	last := len(spec.Partitions) - 1
	var mounted []string
	defer func() {
		for _, d := range slices.Backward(mounted) {
			syscall.Unmount(d, 0)
		}
	}()
	for i, p := range spec.Partitions[:last] {
		if p.Type != "vfat" { // the ext4 ones are mounted already
			continue
		}
		d := filepath.Join(root, p.Path)
		if err := mount(parts[i], d, p.Type); err != nil {
			return err
		}
		mounted = append(mounted, d)
	}
	// Rocky's Ignition always relabels what it wrote with the system's
	// SELinux policy (SELINUX=disabled doesn't stop it), so the image
	// needs one.
	if _, ok := selinuxType(root); !ok {
		return errors.New("firstBoot: ignition: the image has no SELinux policy (/etc/selinux/config and its file_contexts), which Rocky's Ignition needs to relabel the files it writes")
	}
	fmt.Fprintln(out, "stage0 agent: Ignition: files, users and units")
	if err := runTool(out, "ignition", "--root="+root, "--platform=metal", "--stage=files", "--log-to-stdout"); err != nil {
		return err
	}
	syscall.Sync()
	return nil
}

// selinuxType returns the SELinux policy type the root's
// /etc/selinux/config names, when that policy's file contexts are there.
func selinuxType(root string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(root, "etc", "selinux", "config"))
	if err != nil {
		return "", false
	}
	typ := ""
	for l := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "SELINUXTYPE="); ok {
			typ = strings.TrimSpace(v)
		}
	}
	if typ == "" {
		return "", false
	}
	_, err = os.Stat(filepath.Join(root, "etc", "selinux", typ, "contexts", "files", "file_contexts"))
	return typ, err == nil
}

// selinuxOn reports whether the root's SELinux is on (a policy, and
// SELINUX enforcing or permissive).
func selinuxOn(root string) bool {
	if _, ok := selinuxType(root); !ok {
		return false
	}
	b, _ := os.ReadFile(filepath.Join(root, "etc", "selinux", "config"))
	for l := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "SELINUX="); ok {
			return strings.TrimSpace(v) != "disabled"
		}
	}
	return false
}
