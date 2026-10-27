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
		// An SD card has its alias only in uevent; a PCI device has both.
		"sys/devices/platform/mmc/mmc0/mmc0:0001/uevent": "DRIVER=\nMMC_TYPE=SD\nMODALIAS=mmc:block\n",
		"sys/devices/pci0000:00/0000:00:01.0/uevent":     "PCI_ID=1AF4:1000\nMODALIAS=pci:v00001AF4d00001000sv00001AF4sd00000001bc02sc00i00\n",
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
		"mmc:block",
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

func TestColdplug(t *testing.T) {
	// The PCIe bridge's driver brings up the devices behind it.
	devs := []string{"pci:bridge"}
	var loads [][]string
	load := func(a []string) {
		loads = append(loads, a)
		if slices.Contains(a, "pci:bridge") {
			devs = append(devs, "of:ethernet", "of:mmc")
		}
	}
	n := coldplug(func() []string { return devs }, load, func() {})
	if n != 3 || len(loads) != 2 || !slices.Equal(loads[1], []string{"of:ethernet", "of:mmc"}) {
		t.Errorf("n = %d, loads %q", n, loads)
	}
}
