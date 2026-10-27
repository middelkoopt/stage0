package agent

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

func TestParsePlan(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	tests := []struct {
		cmdline string
		want    *Plan
		err     string
	}{
		{"console=ttyAMA10,115200", nil, ""},
		{"console=ttyAMA10 stage0.disk=mmcblk0 stage0.sha256=" + sum, &Plan{Disk: "mmcblk0", SHA256: sum}, ""},
		{"stage0.disk=mmcblk0", nil, "stage0.sha256"},
		{"stage0.sha256=" + sum, nil, "stage0.disk"},
		{"stage0.disk=../sda stage0.sha256=" + sum, nil, "disk name"},
		{"stage0.disk=mmcblk0 stage0.sha256=xyz", nil, "sha256"},
		{"ip=dhcp stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.url=http://10.0.3.1/rocky.raw.xz",
			&Plan{Disk: "mmcblk0", SHA256: sum, URL: "http://10.0.3.1/rocky.raw.xz"}, ""},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.url=ftp://x/y", nil, "http://"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.seed=http://10.0.3.1/seed.cpio",
			&Plan{Disk: "mmcblk0", SHA256: sum, Seed: "http://10.0.3.1/seed.cpio"}, ""},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.seed=/seed.cpio", nil, "http://"},
		// A container: its layout's URL, and the manifest's digest.
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://10.0.3.1/oci stage0.layout=http://10.0.3.1/layout.json",
			&Plan{Disk: "mmcblk0", SHA256: sum, OCI: "http://10.0.3.1/oci", Layout: "http://10.0.3.1/layout.json"}, ""},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=/oci stage0.layout=http://a/l", nil, "http://"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://a/oci stage0.layout=http://a/l stage0.url=http://a/x.xz", nil, "not both"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://a/oci", nil, "go together"},
		{"stage0.disk=mmcblk0 stage0.sha256=" + sum + " stage0.oci=http://a/oci stage0.layout=/l", nil, "http://"},
	}
	for _, tt := range tests {
		got, err := ParsePlan(tt.cmdline)
		switch {
		case tt.err != "":
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%q: err = %v, want one mentioning %q", tt.cmdline, err, tt.err)
			}
		case err != nil:
			t.Errorf("%q: %v", tt.cmdline, err)
		case (got == nil) != (tt.want == nil) || got != nil && *got != *tt.want:
			t.Errorf("%q: plan %+v, want %+v", tt.cmdline, got, tt.want)
		}
	}
}

// xzFile writes data xz-compressed and returns its path and SHA-256.
func xzFile(t *testing.T, data []byte) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "image.raw.xz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return p, hex.EncodeToString(sum[:])
}

func TestInstall(t *testing.T) {
	image := bytes.Repeat([]byte("stage0 image block\n"), 100000) // 1.9 MB
	payload, sum := xzFile(t, image)
	disk := filepath.Join(t.TempDir(), "mmcblk0")
	if err := os.WriteFile(disk, make([]byte, 4<<20), 0o600); err != nil { // a 4 MB "disk"
		t.Fatal(err)
	}
	var progress bytes.Buffer
	res, err := Install(payload, disk, sum, &progress)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(disk)
	if !bytes.Equal(got[:len(image)], image) {
		t.Error("disk does not start with the image")
	}
	if res.Bytes != int64(len(image)) {
		t.Errorf("result %+v, want %d bytes", res, len(image))
	}
}

func TestInstallRefusesWrongChecksum(t *testing.T) {
	payload, _ := xzFile(t, []byte("image"))
	disk := filepath.Join(t.TempDir(), "mmcblk0")
	os.WriteFile(disk, []byte("untouched"), 0o600)
	_, err := Install(payload, disk, strings.Repeat("00", 32), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("err = %v, want a checksum error", err)
	}
	if got, _ := os.ReadFile(disk); string(got) != "untouched" {
		t.Errorf("disk was written: %q", got)
	}
}

func TestInstallRefusesTooBigImage(t *testing.T) {
	payload, sum := xzFile(t, make([]byte, 2<<20))
	disk := filepath.Join(t.TempDir(), "mmcblk0")
	os.WriteFile(disk, make([]byte, 1<<20), 0o600)
	if _, err := Install(payload, disk, sum, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want image larger than disk", err)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rocky.raw.xz" {
			http.NotFound(w, r)
			return
		}
		w.Write(bytes.Repeat([]byte("x"), 1000))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "payload")
	n, err := Fetch(srv.URL+"/rocky.raw.xz", dst, io.Discard)
	if err != nil || n != 1000 {
		t.Fatalf("Fetch = %d, %v", n, err)
	}
	if b, _ := os.ReadFile(dst); len(b) != 1000 {
		t.Errorf("payload has %d bytes", len(b))
	}
	if _, err := Fetch(srv.URL+"/missing", dst, io.Discard); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: err = %v", err)
	}
}

func TestInstallQcow2(t *testing.T) {
	gunzip := func(name string) []byte {
		f, err := os.Open(filepath.Join("..", "qcow2", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(z)
		return b
	}
	img, want := gunzip("zlib.qcow2.gz"), gunzip("disk.raw.gz")
	payload := filepath.Join(t.TempDir(), "image.qcow2")
	os.WriteFile(payload, img, 0o644)
	sum := sha256.Sum256(img)
	disk := filepath.Join(t.TempDir(), "vda")
	os.WriteFile(disk, make([]byte, 8<<20), 0o600)
	res, err := Install(payload, disk, hex.EncodeToString(sum[:]), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(disk)
	if !bytes.Equal(got[:len(want)], want) || res.Bytes != int64(len(want)) {
		t.Errorf("disk differs from the image (result %+v)", res)
	}
}
