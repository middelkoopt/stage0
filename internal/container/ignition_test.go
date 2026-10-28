package container

import (
	"encoding/json"
	"strings"
	"testing"
)

const testIgnition = `{
  "ignition": {"version": "3.4.0"},
  "storage": {
    "disks": [{"device": "/dev/mmcblk0", "wipeTable": true, "partitions": [
      {"label": "piboot", "number": 1, "sizeMiB": 1024},
      {"label": "pivar", "number": 2, "sizeMiB": 8192},
      {"label": "piroot", "number": 3, "sizeMiB": 0}
    ]}],
    "filesystems": [
      {"device": "/dev/disk/by-partlabel/piroot", "format": "ext4", "path": "/", "wipeFilesystem": true},
      {"device": "/dev/disk/by-partlabel/piboot", "format": "vfat", "path": "/boot/efi"},
      {"device": "/dev/disk/by-partlabel/pivar", "format": "ext4", "path": "/var"}
    ]
  }
}`

func TestFromIgnition(t *testing.T) {
	ps, err := FromIgnition([]byte(testIgnition))
	if err != nil {
		t.Fatal(err)
	}
	want := []Partition{
		{Name: "piboot", Size: "1024MiB", Type: "vfat", Path: "/boot/efi", Bytes: 1 << 30},
		{Name: "pivar", Size: "8192MiB", Type: "ext4", Path: "/var", Bytes: 8 << 30},
		{Name: "piroot", Type: "ext4", Path: "/"},
	}
	if len(ps) != len(want) {
		t.Fatalf("partitions %+v", ps)
	}
	for i := range want {
		if ps[i] != want[i] {
			t.Errorf("partition %d = %+v, want %+v", i, ps[i], want[i])
		}
	}
}

func TestFromIgnitionErrors(t *testing.T) {
	for _, tt := range []struct{ name, edit, err string }{
		{"v2", `"version": "3.4.0"`, "version"},
		{"xfs", `"format": "ext4", "path": "/var"`, "xfs"},
		{"no filesystem", `{"device": "/dev/disk/by-partlabel/pivar", "format": "ext4", "path": "/var"}`, "pivar"},
		{"luks", `"filesystems"`, "luks"},
	} {
		in := testIgnition
		switch tt.name {
		case "v2":
			in = strings.Replace(in, tt.edit, `"version": "2.2.0"`, 1)
		case "xfs":
			in = strings.Replace(in, tt.edit, `"format": "xfs", "path": "/var"`, 1)
		case "no filesystem":
			in = strings.Replace(in, ",\n      "+tt.edit, "", 1)
		case "luks":
			in = strings.Replace(in, tt.edit, `"luks": [{"name": "root", "device": "/dev/disk/by-partlabel/piroot"}], "filesystems"`, 1)
		}
		if _, err := FromIgnition([]byte(in)); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s: err = %v, want one mentioning %q", tt.name, err, tt.err)
		}
	}
	two := strings.Replace(testIgnition, `"disks": [{`, `"disks": [{"device": "/dev/sda"}, {`, 1)
	if _, err := FromIgnition([]byte(two)); err == nil || !strings.Contains(err.Error(), "one disk") {
		t.Errorf("two disks: err = %v", err)
	}
}

func TestResolveIgnitionLabel(t *testing.T) {
	labels := map[string]string{
		"pi.provision.ignition":  testIgnition,
		"pi.provision.installer": testLabels["pi.provision.installer"],
	}
	s, ig, err := Resolve(labels, Layout{}, nil, Layout{})
	if err != nil || len(s.Partitions) != 3 || s.Partitions[1].Path != "/var" || !s.Ignition || string(ig) != testIgnition {
		t.Errorf("Resolve = %+v, %v", s, err)
	}
	// A node's partition list wins over the label's Ignition config.
	s, ig, err = Resolve(labels, Layout{}, nil, Layout{Partitions: []Partition{{Name: "b", Size: "1GiB", Type: "vfat", Path: "/boot/efi"}, {Name: "r", Type: "ext4", Path: "/"}}})
	if err != nil || s.Ignition || ig != nil {
		t.Errorf("node over label: %+v, %q, %v", s, ig, err)
	}
	labels["pi.provision.partition"] = testLabels["pi.provision.partition"]
	if _, _, err := Resolve(labels, Layout{}, nil, Layout{}); err == nil {
		t.Error("both partition and ignition labels: want error")
	}
}

func TestForDisk(t *testing.T) {
	b, err := ForDisk([]byte(testIgnition), "/dev/sda")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Ignition struct{ Version string }
		Storage  struct {
			Disks []struct {
				Device    string
				WipeTable bool
			}
		}
	}
	if err := json.Unmarshal(b, &c); err != nil || c.Storage.Disks[0].Device != "/dev/sda" || !c.Storage.Disks[0].WipeTable || c.Ignition.Version != "3.4.0" {
		t.Errorf("ForDisk: %s", b)
	}
	// The FAT boot partition gets the type the Pi boots from; others none.
	if strings.Count(string(b), `"typeGuid": "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"`) != 1 {
		t.Errorf("boot partition type: %s", b)
	}
	if strings.Count(string(b), `"wipeFilesystem": true`) != 3 {
		t.Errorf("wipeFilesystem not on every filesystem: %s", b)
	}
	if ps, err := FromIgnition(b); err != nil || len(ps) != 3 {
		t.Errorf("the rewritten config: %v", err)
	}
}

func TestForDiskKeepsType(t *testing.T) {
	in := strings.Replace(testIgnition, `{"label": "piboot", "number": 1, "sizeMiB": 1024}`,
		`{"label": "piboot", "number": 1, "sizeMiB": 1024, "typeGuid": "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"}`, 1)
	b, err := ForDisk([]byte(in), "/dev/sda")
	if err != nil || !strings.Contains(string(b), "c12a7328-f81f-11d2-ba4b-00a0c93ec93b") || strings.Contains(string(b), basicData) {
		t.Errorf("a given type was replaced: %s, %v", b, err)
	}
}

func TestResolveFirstBootIgnition(t *testing.T) {
	// A node's Ignition config without storage configures first boot only:
	// the layout still comes from the labels.
	files := `{"ignition": {"version": "3.4.0"}, "passwd": {"users": [{"name": "admin"}]}}`
	s, ig, err := Resolve(testLabels, Layout{}, nil, Layout{Ignition: []byte(files)})
	if err != nil || s.Ignition || len(s.Partitions) != 2 || string(ig) != files {
		t.Errorf("Resolve = %+v, %q, %v", s, ig, err)
	}
	// With a partitioning config winning too, two configs: refused.
	if _, _, err := Resolve(map[string]string{"pi.provision.ignition": testIgnition, "pi.provision.installer": testLabels["pi.provision.installer"]},
		Layout{}, nil, Layout{Ignition: []byte(files)}); err == nil || !strings.Contains(err.Error(), "one") {
		t.Errorf("two Ignition configs: err = %v", err)
	}
	if b, err := ForDisk([]byte(files), "/dev/sda"); err != nil || string(b) != files {
		t.Errorf("ForDisk changed a config without disks: %s, %v", b, err)
	}
}
