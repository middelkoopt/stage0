package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ignition is the part of an Ignition config (spec v3,
// https://coreos.github.io/ignition/) a layout comes from: storage.
type ignition struct {
	Ignition struct {
		Version string `json:"version"`
	} `json:"ignition"`
	Storage struct {
		Disks []struct {
			Device     string `json:"device"`
			Partitions []struct {
				Label   string `json:"label"`
				SizeMiB *int64 `json:"sizeMiB"`
			} `json:"partitions"`
		} `json:"disks"`
		Filesystems []struct {
			Device string `json:"device"`
			Format string `json:"format"`
			Path   string `json:"path"`
		} `json:"filesystems"`
		Raid []json.RawMessage `json:"raid"`
		Luks []json.RawMessage `json:"luks"`
	} `json:"storage"`
}

// FromIgnition reads a layout from an Ignition config's storage: the one
// disk's partitions in order, each with the filesystem on
// /dev/disk/by-partlabel/<its label> (vfat or ext4, with a mount path);
// size 0 or none fills the disk. The disk's device is ignored: stage0
// installs on the node's disk. The rest of the config (files, users,
// units) isn't used. The layout is checked as CheckPartitions does.
func FromIgnition(b []byte) ([]Partition, error) {
	var ig ignition
	if err := json.Unmarshal(b, &ig); err != nil {
		return nil, fmt.Errorf("ignition: %w", err)
	}
	st := ig.Storage
	switch {
	case !strings.HasPrefix(ig.Ignition.Version, "3."):
		return nil, fmt.Errorf("ignition: version %q: want spec 3.x", ig.Ignition.Version)
	case len(st.Raid) > 0 || len(st.Luks) > 0:
		return nil, errors.New("ignition: raid and luks aren't supported")
	case len(st.Disks) != 1:
		return nil, fmt.Errorf("ignition: want one disk in storage.disks, have %d", len(st.Disks))
	}
	fs := map[string]int{} // partition label -> filesystem index
	for i, f := range st.Filesystems {
		label, ok := strings.CutPrefix(f.Device, "/dev/disk/by-partlabel/")
		if !ok {
			return nil, fmt.Errorf("ignition: filesystem on %s: name it by /dev/disk/by-partlabel/<label>", f.Device)
		}
		fs[label] = i
	}
	var out []Partition
	for _, p := range st.Disks[0].Partitions {
		i, ok := fs[p.Label]
		if !ok {
			return nil, fmt.Errorf("ignition: partition %q has no filesystem", p.Label)
		}
		f := st.Filesystems[i]
		if f.Format != "vfat" && f.Format != "ext4" {
			return nil, fmt.Errorf("ignition: filesystem %s: format %q (vfat or ext4)", p.Label, f.Format)
		}
		part := Partition{Name: p.Label, Type: f.Format, Path: f.Path}
		if p.SizeMiB != nil && *p.SizeMiB > 0 {
			part.Size = fmt.Sprintf("%dMiB", *p.SizeMiB)
		}
		out = append(out, part)
		delete(fs, p.Label)
	}
	if len(fs) > 0 {
		left := slices.Sorted(maps.Keys(fs))
		return nil, fmt.Errorf("ignition: filesystem on %q, which isn't a partition of the disk", left[0])
	}
	if err := CheckPartitions(out); err != nil {
		return nil, fmt.Errorf("ignition: %w", err)
	}
	return out, nil
}

// basicData is the GPT type Microsoft basic data, which the Pi firmware
// boots from.
const basicData = "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"

// ForDisk returns the Ignition config for installing on dev (e.g.
// /dev/mmcblk0): its one disk's device set to dev, wipeTable and every
// filesystem's wipeFilesystem on, as stage0 installs afresh, and FAT
// partitions without a type typed basic data. The rest stays as given.
func ForDisk(config []byte, dev string) ([]byte, error) {
	var c map[string]any
	if err := json.Unmarshal(config, &c); err != nil {
		return nil, fmt.Errorf("ignition: %w", err)
	}
	st, _ := c["storage"].(map[string]any)
	disks, _ := st["disks"].([]any)
	if len(disks) == 0 { // first boot only: nothing to install on
		return config, nil
	}
	if len(disks) != 1 {
		return nil, fmt.Errorf("ignition: want one disk in storage.disks, have %d", len(disks))
	}
	d, ok := disks[0].(map[string]any)
	if !ok {
		return nil, errors.New("ignition: storage.disks[0] is not an object")
	}
	d["device"], d["wipeTable"] = dev, true
	// New partitions at the old offsets still hold the old filesystems,
	// which Ignition would keep if their format matches.
	fss, _ := st["filesystems"].([]any)
	vfat := map[string]bool{} // partition labels with a FAT filesystem
	for _, f := range fss {
		if fm, ok := f.(map[string]any); ok {
			fm["wipeFilesystem"] = true
			if fm["format"] == "vfat" {
				dev, _ := fm["device"].(string)
				vfat[strings.TrimPrefix(dev, "/dev/disk/by-partlabel/")] = true
			}
		}
	}
	// A Pi boots only from a FAT partition typed Microsoft basic data
	// (Ignition's default type is Linux filesystem: "GPT: No bootable
	// partitions" on k52); a type the config gives stays.
	parts, _ := d["partitions"].([]any)
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			label, _ := pm["label"].(string)
			if _, set := pm["typeGuid"]; vfat[label] && !set {
				pm["typeGuid"] = basicData
			}
		}
	}
	return json.MarshalIndent(c, "", "  ")
}

// hasDisks reports whether an Ignition config partitions a disk.
func hasDisks(config []byte) bool {
	var ig ignition
	return len(config) > 0 && json.Unmarshal(config, &ig) == nil && len(ig.Storage.Disks) > 0
}
