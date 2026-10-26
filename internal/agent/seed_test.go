package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/middelkoopt/stage0/internal/cpio"
)

func seedArchive(t *testing.T, files map[string]string, dirs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	for _, d := range dirs {
		if err := w.Dir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := w.File(name, 0o644, int64(len(body)), strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestApplySeed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cmdline.txt"), []byte("console=ttyAMA0,115200 root=LABEL=RPIROOT"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("untouched"), 0o644)
	a := seedArchive(t, map[string]string{
		"cmdline.txt":    "console=serial0,115200 root=LABEL=RPIROOT",
		"user/user-data": "#cloud-config\n",
	}, "user")
	names, err := ApplySeed(bytes.NewReader(a), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cmdline.txt")); string(b) != "console=serial0,115200 root=LABEL=RPIROOT" {
		t.Errorf("cmdline.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "user/user-data")); string(b) != "#cloud-config\n" {
		t.Errorf("user-data = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.txt")); string(b) != "untouched" {
		t.Errorf("config.txt = %q; files not in the seed stay as they are", b)
	}
	if strings.Join(names, ",") == "" {
		t.Error("no names reported")
	}
}

func TestApplySeedStaysInside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "boot")
	os.Mkdir(target, 0o755)
	a := seedArchive(t, map[string]string{"../escape.txt": "x"})
	if _, err := ApplySeed(bytes.NewReader(a), target); err == nil {
		t.Error("wrote outside the target")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Error("escape.txt exists")
	}
}

func TestPartitionDevice(t *testing.T) {
	for disk, want := range map[string]string{
		"mmcblk0": "mmcblk0p1", "nvme0n1": "nvme0n1p1", "sda": "sda1", "vda": "vda1",
	} {
		if got := partition(disk, 1); got != want {
			t.Errorf("partition(%s, 1) = %s, want %s", disk, got, want)
		}
	}
}
