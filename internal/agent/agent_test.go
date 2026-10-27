package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fakeRoot builds the /proc and /sys files Gather reads, as a Pi 5 with an
// SD card and an NVMe drive shows them.
func fakeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"proc/sys/kernel/osrelease":                  "6.12.103-1.el10.altarch.aarch64+4k\n",
		"proc/cmdline":                               "console=ttyAMA10,115200\n",
		"proc/meminfo":                               "MemTotal:        8245824 kB\nMemFree:  100 kB\n",
		"proc/filesystems":                           "nodev\tsysfs\nnodev\ttmpfs\n\text4\n\tvfat\n\txfs\n",
		"sys/firmware/devicetree/base/model":         "Raspberry Pi 5 Model B Rev 1.0\x00",
		"sys/firmware/devicetree/base/serial-number": "a8a3b55a563cea77\x00",
		"sys/block/mmcblk0/size":                     "62333952\n",
		"sys/block/mmcblk0/removable":                "0\n",
		"sys/block/mmcblk0/device/name":              "SD64G\n",
		"sys/block/nvme0n1/size":                     "500118192\n",
		"sys/block/nvme0n1/removable":                "0\n",
		"sys/block/nvme0n1/device/model":             "Samsung SSD 980 250GB                   \n",
		"sys/block/loop0/size":                       "0\n",
		"sys/block/ram0/size":                        "131072\n",
		"sys/class/net/end0/address":                 "d8:3a:dd:c5:c1:f3\n",
		"sys/class/net/end0/operstate":               "up\n",
		"sys/class/net/lo/address":                   "00:00:00:00:00:00\n",
		"sys/class/net/lo/operstate":                 "unknown\n",
		"payload":                                    "hello",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGather(t *testing.T) {
	r, err := Gather(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	want := Report{
		Model:       "Raspberry Pi 5 Model B Rev 1.0",
		Serial:      "a8a3b55a563cea77",
		Kernel:      "6.12.103-1.el10.altarch.aarch64+4k",
		Cmdline:     "console=ttyAMA10,115200",
		MemTotalMB:  8052,
		Filesystems: []string{"ext4", "vfat", "xfs"},
		Disks: []Disk{
			{Name: "mmcblk0", Bytes: 62333952 * 512, Model: "SD64G"},
			{Name: "nvme0n1", Bytes: 500118192 * 512, Model: "Samsung SSD 980 250GB"},
		},
		Interfaces: []Interface{{Name: "end0", MAC: "d8:3a:dd:c5:c1:f3", State: "up"}},
		Payload: &Payload{
			Bytes:  5,
			SHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("report:\n got %+v\nwant %+v", r, want)
	}
}

func TestGatherNoPayload(t *testing.T) {
	root := fakeRoot(t)
	os.Remove(filepath.Join(root, "payload"))
	r, err := Gather(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Payload != nil {
		t.Errorf("payload = %+v, want none", r.Payload)
	}
}

func TestSettle(t *testing.T) {
	// Disks appear a moment after boot; settle waits until the list stops
	// changing.
	root := fakeRoot(t)
	nvme := filepath.Join(root, "sys/block/nvme0n1")
	hidden := filepath.Join(root, "nvme-later")
	if err := os.Rename(nvme, hidden); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Rename(hidden, nvme)
	}()
	r, err := settle(root, 10*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Disks) != 2 {
		t.Errorf("disks = %+v, want both after settling", r.Disks)
	}
}

func TestWaitFor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mmcblk0")
	tries := 0
	waitFor(p, 5*time.Second, func() {
		if tries++; tries == 2 {
			os.WriteFile(p, nil, 0o644)
		}
	})
	if tries != 2 {
		t.Errorf("tries = %d", tries)
	}
}
