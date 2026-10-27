package container

import (
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
	s, err := Resolve(labels, nil, nil, nil)
	if err != nil || len(s.Partitions) != 3 || s.Partitions[1].Path != "/var" {
		t.Errorf("Resolve = %+v, %v", s, err)
	}
	labels["pi.provision.partition"] = testLabels["pi.provision.partition"]
	if _, err := Resolve(labels, nil, nil, nil); err == nil {
		t.Error("both partition and ignition labels: want error")
	}
}
