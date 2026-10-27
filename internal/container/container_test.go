package container

import (
	"encoding/json"
	"strings"
	"testing"
)

var testLabels = map[string]string{
	"pi.provision.partition": `[{"name":"piboot","size":"512MiB","type":"vfat","path":"/boot/efi"},{"name":"piroot","type":"ext4","path":"/"}]`,
	"pi.provision.installer": `{"kernel":"/boot/efi/kernel8.img","initramfs":"/boot/stage0-installer.img"}`,
}

func TestResolveLabels(t *testing.T) {
	s, err := Resolve(testLabels, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Partitions) != 2 || s.Partitions[0].Bytes != 512<<20 || s.Partitions[1].Bytes != 0 ||
		s.Installer.Kernel != "/boot/efi/kernel8.img" || s.Installer.Initramfs != "/boot/stage0-installer.img" {
		t.Errorf("spec %+v", s)
	}
	if b := s.Boot(); b.Path != "/boot/efi" {
		t.Errorf("Boot() = %+v", b)
	}
	if rel, err := s.InBoot(s.Installer.Kernel); err != nil || rel != "kernel8.img" {
		t.Errorf("InBoot(kernel) = %q, %v", rel, err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	image := []Partition{{Name: "boot", Size: "1GiB", Type: "vfat", Path: "/boot/efi"}, {Name: "root", Type: "ext4", Path: "/"}}
	node := []Partition{{Name: "fw", Size: "256MiB", Type: "vfat", Path: "/boot/efi"},
		{Name: "var", Size: "8GiB", Type: "ext4", Path: "/var"}, {Name: "root", Type: "ext4", Path: "/"}}
	inst := &Installer{Kernel: "/boot/efi/k", Initramfs: "/boot/i"}

	s, err := Resolve(testLabels, image, inst, nil)
	if err != nil || s.Partitions[0].Name != "boot" || s.Partitions[0].Bytes != 1<<30 || s.Installer.Kernel != "/boot/efi/k" {
		t.Errorf("image over labels: %+v, %v", s, err)
	}
	s, err = Resolve(testLabels, image, nil, node)
	if err != nil || len(s.Partitions) != 3 || s.Partitions[1].Bytes != 8<<30 || s.Installer.Kernel != "/boot/efi/kernel8.img" {
		t.Errorf("node over image: %+v, %v", s, err)
	}
	// No labels at all: the image config says it all.
	if _, err := Resolve(nil, image, inst, nil); err != nil {
		t.Errorf("no labels: %v", err)
	}
	if _, err := Resolve(nil, image, nil, nil); err == nil || !strings.Contains(err.Error(), "installer") {
		t.Errorf("no installer anywhere: err = %v", err)
	}
	if _, err := Resolve(nil, nil, inst, nil); err == nil || !strings.Contains(err.Error(), "partitions") {
		t.Errorf("no partitions anywhere: err = %v", err)
	}
}

func TestSpecJSON(t *testing.T) {
	// stage0 serves the resolved spec to the agent as JSON.
	s, _ := Resolve(testLabels, nil, nil, nil)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got Spec
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil || got.Partitions[0].Bytes != 512<<20 {
		t.Errorf("round trip %+v, %v", got, err)
	}
}

func TestCheckPartitions(t *testing.T) {
	for _, tt := range []struct{ parts, err string }{
		{`[{"name":"r","type":"ext4","path":"/"}]`, "boot and a root"},
		{`[{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"xfs","path":"/"}]`, "type"},
		{`[{"name":"b","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/"}]`, "size"},
		{`[{"name":"b","size":"12XB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/"}]`, "size"},
		{`[{"name":"r","type":"ext4","path":"/"},{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"}]`, "first"},
		{`[{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/x"}]`, "root"},
		{`[{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"},{"name":"r","size":"2GiB","type":"ext4","path":"/"}]`, "fills"},
		{`[{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"},{"name":"b","type":"ext4","path":"/"}]`, "twice"},
	} {
		var ps []Partition
		if err := json.Unmarshal([]byte(tt.parts), &ps); err != nil {
			t.Fatal(err)
		}
		if err := CheckPartitions(ps); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s: err = %v, want one mentioning %q", tt.parts, err, tt.err)
		}
	}
	if _, err := Resolve(map[string]string{"pi.provision.partition": "not json"}, nil, nil, nil); err == nil {
		t.Error("a bad label: want error")
	}
}
