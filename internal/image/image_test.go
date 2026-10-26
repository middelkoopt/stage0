package image

import (
	"bytes"
	"compress/gzip"
	"context"
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

func gunzip(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testImage is an xz disk image: the GPT fixture with the FAT12 fixture
// in partition 1 (at 1 MiB). It returns the image and its SHA-256.
func testImage(t *testing.T) ([]byte, string) {
	t.Helper()
	disk := gunzip(t, "../ptable/testdata/gpt.img.gz")
	copy(disk[1<<20:], gunzip(t, "../fat/testdata/fat12.img.gz"))
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(disk)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func writeConfig(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "rocky", `
url: https://example.org/Rocky.raw.xz
sha256: `+strings.Repeat("ab", 32)+`
cmdline: console=serial0,115200 root=PARTUUID=x rootwait
ready: 'login: '
`)
	c, err := Load(dir, "rocky")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "rocky" || c.File() != "Rocky.raw.xz" || c.BootPartition != 1 || c.Ready != "login: " {
		t.Errorf("config %+v (file %s)", c, c.File())
	}
}

func TestLoadErrors(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	tests := map[string]string{
		"no url":      "sha256: " + sum + "\ncmdline: x\n",
		"no sha256":   "url: https://e.org/a.raw.xz\ncmdline: x\n",
		"bad sha256":  "url: https://e.org/a.raw.xz\nsha256: xyz\ncmdline: x\n",
		"not raw.xz":  "url: https://e.org/a.iso\nsha256: " + sum + "\ncmdline: x\n",
		"no cmdline":  "url: https://e.org/a.raw.xz\nsha256: " + sum + "\n",
		"bad ready":   "url: https://e.org/a.raw.xz\nsha256: " + sum + "\ncmdline: x\nready: '('\n",
		"unknown key": "url: https://e.org/a.raw.xz\nsha256: " + sum + "\ncmdline: x\nfoo: 1\n",
	}
	for name, body := range tests {
		dir := t.TempDir()
		writeConfig(t, dir, "img", body)
		if _, err := Load(dir, "img"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := Load(t.TempDir(), "../etc/passwd"); err == nil {
		t.Error("path in image name: want error")
	}
}

func TestFetchAndBootFiles(t *testing.T) {
	img, sum := testImage(t)
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Write(img)
	}))
	defer srv.Close()
	c := Config{Name: "test", URL: srv.URL + "/test.raw.xz", SHA256: sum, BootPartition: 1}
	cache := t.TempDir()

	path, err := Fetch(context.Background(), c, cache, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), c, cache, io.Discard); err != nil || gets != 1 {
		t.Errorf("second fetch: err %v, %d downloads (want the cached copy)", err, gets)
	}

	boot := filepath.Join(t.TempDir(), "boot")
	if err := BootFiles(path, c.BootPartition, boot); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(boot, "config.txt")); err != nil || string(b) != "arm_64bit=1\n" {
		t.Errorf("config.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(boot, "overlays", "a-rather-long-overlay-name.dtbo")); err != nil {
		t.Error(err)
	}
}

func TestFetchRefusesWrongChecksum(t *testing.T) {
	img, _ := testImage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(img) }))
	defer srv.Close()
	c := Config{Name: "test", URL: srv.URL + "/test.raw.xz", SHA256: strings.Repeat("00", 32), BootPartition: 1}
	cache := t.TempDir()
	if _, err := Fetch(context.Background(), c, cache, io.Discard); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("err = %v", err)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Errorf("left %d files in the cache", len(ents))
	}
}

func TestBootFilesRealImage(t *testing.T) {
	path := os.Getenv("STAGE0_TEST_IMAGE_XZ")
	if path == "" {
		t.Skip("set STAGE0_TEST_IMAGE_XZ to an xz disk image")
	}
	boot := filepath.Join(t.TempDir(), "boot")
	if err := BootFiles(path, 1, boot); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(boot)
	t.Logf("%d entries in the boot partition", len(ents))
}
