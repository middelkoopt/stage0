// Package container says how to install a container image: its partitions,
// and the kernel and installer initramfs that boot the agent. A container
// can carry them as labels (the lab's pi.provision.* convention); an image
// config and a node can say otherwise (docs/containers.md).
package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Partition is one partition to make, in order on the disk.
type Partition struct {
	Name  string `json:"name" yaml:"name"`                     // its GPT partition name (root=PARTLABEL=…)
	Size  string `json:"size,omitempty" yaml:"size,omitempty"` // e.g. 512MiB; none for the root, which fills the disk
	Type  string `json:"type" yaml:"type"`                     // vfat or ext4
	Path  string `json:"path" yaml:"path"`                     // where it is mounted in the installed system
	Bytes int64  `json:"-" yaml:"-"`                           // Size, set by CheckPartitions; 0 for the root
}

// Installer is what boots the agent: paths in the image.
type Installer struct {
	Kernel    string `json:"kernel" yaml:"kernel"`
	Initramfs string `json:"initramfs" yaml:"initramfs"`
}

// Spec is how to install a container.
type Spec struct {
	Partitions []Partition `json:"partitions"`
	Installer  Installer   `json:"installer"`
}

// Resolve settles the spec: the container's labels, overridden by the
// image config's partitions and installer, the partitions overridden in
// turn by the node's. Any of them may be missing; the result is checked.
func Resolve(labels map[string]string, imageParts []Partition, imageInstaller *Installer, nodeParts []Partition) (Spec, error) {
	var s Spec
	if v, ok := labels["pi.provision.partition"]; ok {
		if err := json.Unmarshal([]byte(v), &s.Partitions); err != nil {
			return s, fmt.Errorf("label pi.provision.partition: %w", err)
		}
	}
	if v, ok := labels["pi.provision.installer"]; ok {
		if err := json.Unmarshal([]byte(v), &s.Installer); err != nil {
			return s, fmt.Errorf("label pi.provision.installer: %w", err)
		}
	}
	if len(imageParts) > 0 {
		s.Partitions = imageParts
	}
	if imageInstaller != nil {
		s.Installer = *imageInstaller
	}
	if len(nodeParts) > 0 {
		s.Partitions = nodeParts
	}
	s.Partitions = append([]Partition(nil), s.Partitions...) // Validate sets Bytes; leave the callers' alone
	return s, s.Validate()
}

// Validate checks the spec and sets each partition's Bytes.
func (s *Spec) Validate() error {
	if s.Installer.Kernel == "" || s.Installer.Initramfs == "" {
		return errors.New("no installer kernel and initramfs: give the container a pi.provision.installer label or the image config an installer")
	}
	if len(s.Partitions) == 0 {
		return errors.New("no partitions: give the container a pi.provision.partition label, or the image config or node partitions")
	}
	return CheckPartitions(s.Partitions)
}

// CheckPartitions checks a layout and sets each partition's Bytes. The
// first is the Pi's boot partition (vfat, a size); the last is the root
// (ext4 at /, no size: it fills the disk).
func CheckPartitions(ps []Partition) error {
	if len(ps) < 2 {
		return fmt.Errorf("partitions: want a boot and a root partition, have %d", len(ps))
	}
	names := map[string]bool{}
	for i := range ps {
		p := &ps[i]
		last := i == len(ps)-1
		switch {
		case p.Type != "vfat" && p.Type != "ext4":
			return fmt.Errorf("partition %s: type %q: vfat or ext4", p.Name, p.Type)
		case p.Name == "" || !path.IsAbs(p.Path):
			return fmt.Errorf("partition %q: needs a name and an absolute path", p.Name)
		case names[p.Name]:
			return fmt.Errorf("partition %s: named twice", p.Name)
		case i == 0 && p.Type != "vfat":
			return fmt.Errorf("partition %s: the first is the Pi's boot partition, so vfat", p.Name)
		case last && (p.Path != "/" || p.Type != "ext4"):
			return fmt.Errorf("partition %s: the last is the root, ext4 at /", p.Name)
		case last && p.Size != "":
			return fmt.Errorf("partition %s: the root fills the disk, so no size", p.Name)
		case !last:
			n, err := parseSize(p.Size)
			if err != nil {
				return fmt.Errorf("partition %s: size: %w", p.Name, err)
			}
			p.Bytes = n
		}
		names[p.Name] = true
	}
	return nil
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
