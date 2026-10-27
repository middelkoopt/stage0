package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestModaliases(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"sys/devices/pci0000:00/0000:00:01.0/modalias":                 "pci:v00001AF4d00001000sv00001AF4sd00000001bc02sc00i00\n",
		"sys/devices/pci0000:00/0000:00:01.0/virtio0/modalias":         "virtio:d00000001v00001AF4\n",
		"sys/devices/pci0000:00/0000:00:02.0/virtio1/modalias":         "virtio:d00000002v00001AF4\n",
		"sys/devices/platform/empty/modalias":                          "\n",
		"sys/devices/pci0000:00/0000:00:01.0/virtio0/net/eth0/address": "52:54:00:12:34:56\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	got, err := modaliases(filepath.Join(root, "sys/devices"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{
		"pci:v00001AF4d00001000sv00001AF4sd00000001bc02sc00i00",
		"virtio:d00000001v00001AF4",
		"virtio:d00000002v00001AF4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("aliases %q\nwant %q", got, want)
	}
}

func TestFindModprobe(t *testing.T) {
	root := t.TempDir()
	if p := findModprobe(root); p != "" {
		t.Errorf("found %q in an empty root", p)
	}
	os.MkdirAll(filepath.Join(root, "usr/sbin"), 0o755)
	os.WriteFile(filepath.Join(root, "usr/sbin/modprobe"), []byte("#!"), 0o755)
	if p := findModprobe(root); p != filepath.Join(root, "usr/sbin/modprobe") {
		t.Errorf("found %q", p)
	}
}
