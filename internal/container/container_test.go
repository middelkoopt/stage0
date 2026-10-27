package container

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	labels := map[string]string{
		"pi.provision.partition": `[{"name":"piboot","size":"512MiB","type":"vfat","path":"/boot/efi"},{"name":"piroot","type":"ext4","path":"/"}]`,
		"pi.provision.installer": `{"kernel":"/boot/efi/kernel8.img","initramfs":"/boot/stage0-installer.img"}`,
	}
	s, err := Parse(labels)
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

func TestParseErrors(t *testing.T) {
	inst := `{"kernel":"/boot/efi/k","initramfs":"/boot/i"}`
	for _, tt := range []struct{ parts, inst, err string }{
		{"", inst, "pi.provision.partition"},
		{`[{"name":"r","type":"ext4","path":"/"}]`, "", "pi.provision.installer"},
		{`[{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"xfs","path":"/"}]`, inst, "type"},
		{`[{"name":"b","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/"}]`, inst, "size"},
		{`[{"name":"b","size":"12XB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/"}]`, inst, "size"},
		{`[{"name":"r","type":"ext4","path":"/"},{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"}]`, inst, "first"},
		{`[{"name":"b","size":"1GiB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/x"}]`, inst, "root"},
	} {
		labels := map[string]string{}
		if tt.parts != "" {
			labels["pi.provision.partition"] = tt.parts
		}
		if tt.inst != "" {
			labels["pi.provision.installer"] = tt.inst
		}
		if _, err := Parse(labels); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s / %s: err = %v, want one mentioning %q", tt.parts, tt.inst, err, tt.err)
		}
	}
}
