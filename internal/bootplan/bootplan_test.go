package bootplan

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, p := range []Plan{
		{Disk: "mmcblk0", SHA256: sum, URL: "http://10.0.3.1/images/x.raw.xz", Seed: "http://10.0.3.1/seed.cpio", Report: "http://10.0.3.1/stage0/report"},
		{Disk: "mmcblk0", SHA256: sum, OCI: "http://[fd00:10:3::1]/oci", Layout: "http://[fd00:10:3::1]/layout.json", Report: "http://[fd00:10:3::1]/stage0/report"},
		{Disk: "vda", SHA256: sum},
		{Report: "http://10.0.3.1/stage0/report"},
	} {
		got, err := Parse("console=ttyAMA0 " + p.Encode() + " quiet")
		if err != nil || got != p {
			t.Errorf("Parse(Encode(%+v)) = %+v, %v", p, got, err)
		}
	}
}

func TestParse(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	if p, err := Parse("console=ttyAMA10,115200"); err != nil || p.Installs() || p.NeedsNetwork() {
		t.Errorf("no plan: %+v, %v", p, err)
	}
	for _, tt := range []struct{ cmdline, err string }{
		{"stage0.disk=mmcblk0", "stage0.sha256"},
		{"stage0.sha256=" + sum, "stage0.disk"},
		{"stage0.disk=../sda stage0.sha256=" + sum, "disk name"},
		{"stage0.disk=mmcblk0 stage0.sha256=xyz", "sha256"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.url=ftp://x/y", "http://"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.seed=/seed.cpio", "http://"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=/oci stage0.layout=http://a/l", "http://"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://a/oci stage0.layout=http://a/l stage0.url=http://a/x.xz", "not both"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://a/oci", "go together"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://a/oci stage0.layout=/l", "http://"},
		{"stage0.report=ftp://x/r", "http://"},
	} {
		if _, err := Parse(tt.cmdline); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: err = %v, want one mentioning %q", tt.cmdline, err, tt.err)
		}
	}
}

func TestQuestions(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	p, _ := Parse("stage0.disk=vda stage0.sha256=" + sum + " stage0.url=http://[fd00:10:9::1]/images/x.qcow2")
	if !p.Installs() || !p.NeedsNetwork() || !p.IPv6() {
		t.Errorf("%+v: installs %v, network %v, ipv6 %v", p, p.Installs(), p.NeedsNetwork(), p.IPv6())
	}
	p, _ = Parse("stage0.disk=vda stage0.sha256=" + sum)
	if p.NeedsNetwork() || p.IPv6() {
		t.Errorf("payload in the initramfs: %+v", p)
	}
}
