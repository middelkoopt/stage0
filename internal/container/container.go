// Package container reads how to install a container image from its
// labels (the lab's pi.provision.* convention, docs/containers.md): its
// partitions, and the kernel and installer initramfs that boot the agent.
package container

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Partition is one partition to make, in order on the disk.
type Partition struct {
	Name  string `json:"name"` // its GPT partition name (root=PARTLABEL=…)
	Size  string `json:"size,omitempty"`
	Type  string `json:"type"` // vfat or ext4
	Path  string `json:"path"` // where it is mounted in the installed system
	Bytes int64  `json:"-"`    // Size; 0 for the rest of the disk
}

// Installer is what boots the agent: paths in the image.
type Installer struct {
	Kernel    string `json:"kernel"`
	Initramfs string `json:"initramfs"`
}

// Spec is a container's install instructions.
type Spec struct {
	Partitions []Partition
	Installer  Installer
}

// Parse reads the labels. The first partition is the Pi's boot partition
// (vfat, a size); the last is the root (ext4, at /), filling the disk.
func Parse(labels map[string]string) (Spec, error) {
	var s Spec
	pl, ok := labels["pi.provision.partition"]
	if !ok {
		return s, fmt.Errorf("no pi.provision.partition label")
	}
	if err := json.Unmarshal([]byte(pl), &s.Partitions); err != nil {
		return s, fmt.Errorf("pi.provision.partition: %w", err)
	}
	il, ok := labels["pi.provision.installer"]
	if !ok {
		return s, fmt.Errorf("no pi.provision.installer label: the container needs an installer initramfs")
	}
	if err := json.Unmarshal([]byte(il), &s.Installer); err != nil {
		return s, fmt.Errorf("pi.provision.installer: %w", err)
	}
	if s.Installer.Kernel == "" || s.Installer.Initramfs == "" {
		return s, fmt.Errorf("pi.provision.installer: needs kernel and initramfs")
	}
	if len(s.Partitions) < 2 {
		return s, fmt.Errorf("pi.provision.partition: want a boot and a root partition, have %d", len(s.Partitions))
	}
	for i := range s.Partitions {
		p := &s.Partitions[i]
		last := i == len(s.Partitions)-1
		switch {
		case p.Type != "vfat" && p.Type != "ext4":
			return s, fmt.Errorf("partition %s: type %q: vfat or ext4", p.Name, p.Type)
		case p.Name == "" || !path.IsAbs(p.Path):
			return s, fmt.Errorf("partition %q: needs a name and an absolute path", p.Name)
		case i == 0 && p.Type != "vfat":
			return s, fmt.Errorf("partition %s: the first is the Pi's boot partition, so vfat", p.Name)
		case last && (p.Path != "/" || p.Type != "ext4"):
			return s, fmt.Errorf("partition %s: the last is the root, ext4 at /", p.Name)
		case last && p.Size != "":
			return s, fmt.Errorf("partition %s: the root fills the disk, so no size", p.Name)
		case !last:
			n, err := parseSize(p.Size)
			if err != nil {
				return s, fmt.Errorf("partition %s: size: %w", p.Name, err)
			}
			p.Bytes = n
		}
	}
	return s, nil
}

// Boot is the boot partition.
func (s Spec) Boot() Partition { return s.Partitions[0] }

// InBoot returns p (a path in the image) relative to the boot
// partition's mount point, or an error if it isn't under it.
func (s Spec) InBoot(p string) (string, error) {
	b := s.Partitions[0].Path
	rel, ok := strings.CutPrefix(path.Clean(p), strings.TrimSuffix(b, "/")+"/")
	if !ok {
		return "", fmt.Errorf("%s is not on the boot partition (%s)", p, b)
	}
	return rel, nil
}

// parseSize reads "512MiB", "1GiB" (binary units) or a byte count.
func parseSize(v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("missing")
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		n      int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}} {
		if num, ok := strings.CutSuffix(v, u.suffix); ok {
			v, mult = num, u.n
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: a positive number of bytes, KiB, MiB or GiB", v)
	}
	return n * mult, nil
}
