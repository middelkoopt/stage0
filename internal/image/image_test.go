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
		"no url":       "sha256: " + sum + "\ncmdline: x\n",
		"no sha256":    "url: https://e.org/a.raw.xz\ncmdline: x\n",
		"bad sha256":   "url: https://e.org/a.raw.xz\nsha256: xyz\ncmdline: x\n",
		"not xz":       "url: https://e.org/a.iso\nsha256: " + sum + "\ncmdline: x\n",
		"bad bootFile": "url: https://e.org/a.raw.xz\nsha256: " + sum + "\ncmdline: x\nbootFiles: {../x: y}\n",
		"no cmdline":   "url: https://e.org/a.raw.xz\nsha256: " + sum + "\n",
		"bad ready":    "url: https://e.org/a.raw.xz\nsha256: " + sum + "\ncmdline: x\nready: '('\n",
		"unknown key":  "url: https://e.org/a.raw.xz\nsha256: " + sum + "\ncmdline: x\nfoo: 1\n",
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

func TestLoadConfigLines(t *testing.T) {
	dir := t.TempDir()
	sum := strings.Repeat("ab", 32)
	writeConfig(t, dir, "pi", "url: https://e.org/a.raw.xz\nsha256: "+sum+"\ncmdline: x\nconfig: [enable_uart=1, dtoverlay=disable-bt]\n")
	c, err := Load(dir, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Config, ",") != "enable_uart=1,dtoverlay=disable-bt" {
		t.Errorf("config %v", c.Config)
	}
	writeConfig(t, dir, "bad", "url: https://e.org/a.raw.xz\nsha256: "+sum+"\ncmdline: x\nconfig: [\"a=1\\nb=2\"]\n")
	if _, err := Load(dir, "bad"); err == nil {
		t.Error("multi-line config entry: want error")
	}
}

func TestEnsureConfig(t *testing.T) {
	base := "arm_64bit=1\nauto_initramfs=1\n[pi4]\nenable_uart=1\n"
	got := string(EnsureConfig([]byte(base), []string{"arm_64bit=1", "enable_uart=1", "dtoverlay=disable-bt"}))
	want := base + "\n# Added by stage0.\n[all]\nenable_uart=1\ndtoverlay=disable-bt\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := string(EnsureConfig([]byte(base), nil)); got != base {
		t.Errorf("nothing to add changed the file: %q", got)
	}
	if got := string(EnsureConfig([]byte("arm_64bit=1"), []string{"enable_uart=1"})); !strings.HasPrefix(got, "arm_64bit=1\n\n#") {
		t.Errorf("no final newline: %q", got)
	}
}

func TestSetCloudInitHostname(t *testing.T) {
	in := "#cloud-config\n# comment\nhostname: almalinux.local\nssh_pwauth: false\nusers:\n  - name: almalinux\n    hostname: nested-stays\n"
	want := "#cloud-config\n# comment\nhostname: k400\nssh_pwauth: false\nusers:\n  - name: almalinux\n    hostname: nested-stays\ndisable_root: false\n"
	if got := string(SetCloudInitHostname([]byte(in), "k400")); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := string(SetCloudInitHostname([]byte("#cloud-config\nssh_pwauth: false\ndisable_root: true\n"), "k400")); got != "#cloud-config\nssh_pwauth: false\ndisable_root: false\nhostname: k400\n" {
		t.Errorf("no hostname key: got %q", got)
	}
}

func TestLoadBootFiles(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "raspios", "url: https://e.org/2026-10-06-raspios-trixie-arm64-lite.img.xz\nsha256: "+
		strings.Repeat("ab", 32)+"\ncmdline: x\nbootFiles:\n  ssh: \"\"\n")
	c, err := Load(dir, "raspios")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := c.BootFiles["ssh"]; !ok || v != "" || c.File() != "2026-10-06-raspios-trixie-arm64-lite.img.xz" {
		t.Errorf("config %+v", c)
	}
}

func TestLoadVMImage(t *testing.T) {
	dir := t.TempDir()
	sum := strings.Repeat("ab", 32)
	writeConfig(t, dir, "rocky-vm", "url: https://e.org/Rocky.aarch64.qcow2\nsha256: "+sum+
		"\nnocloud: true\nkernel: {url: https://e.org/vmlinuz, sha256: "+sum+"}\ninitrd: {url: https://e.org/initrd.img, sha256: "+sum+"}\n")
	c, err := Load(dir, "rocky-vm")
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoCloud || c.Kernel == nil || c.Kernel.File() != "vmlinuz" || c.Initrd.SHA256 != sum || !c.VM() {
		t.Errorf("config %+v", c)
	}
	for name, body := range map[string]string{
		"kernel without initrd": "url: https://e.org/a.qcow2\nsha256: " + sum + "\nkernel: {url: https://e.org/k, sha256: " + sum + "}\n",
		"bad kernel sha":        "url: https://e.org/a.qcow2\nsha256: " + sum + "\nkernel: {url: https://e.org/k, sha256: x}\ninitrd: {url: https://e.org/i, sha256: " + sum + "}\n",
		"no cmdline, no kernel": "url: https://e.org/a.raw.xz\nsha256: " + sum + "\n",
	} {
		writeConfig(t, dir, "bad", body)
		if _, err := Load(dir, "bad"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestFetchFile(t *testing.T) {
	body := []byte("kernel bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	sum := sha256.Sum256(body)
	f := File{URL: srv.URL + "/vmlinuz", SHA256: hex.EncodeToString(sum[:])}
	p, err := FetchFile(context.Background(), f, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "kernel bytes" || filepath.Base(p) != "vmlinuz" {
		t.Errorf("%s: %q", p, b)
	}
}

func TestLoadBootDir(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "ubuntu", "url: https://e.org/u.img.xz\nsha256: "+strings.Repeat("ab", 32)+
		"\ncmdline: x\nbootDir: current\ninitramfs: current/initrd.img\n")
	c, err := Load(dir, "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if c.BootDir != "current" || c.Initramfs != "current/initrd.img" {
		t.Errorf("config %+v", c)
	}
	writeConfig(t, dir, "bad", "url: https://e.org/u.img.xz\nsha256: "+strings.Repeat("ab", 32)+"\ncmdline: x\nbootDir: ../x\n")
	if _, err := Load(dir, "bad"); err == nil {
		t.Error("bootDir outside the partition: want error")
	}
}

func TestLoadContainer(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "rocky", "container: rocky10-rpi\ncmdline: root=PARTLABEL=piroot\n")
	c, err := Load(dir, "rocky")
	if err != nil {
		t.Fatal(err)
	}
	if c.Container != "rocky10-rpi" || c.VM() {
		t.Errorf("config %+v", c)
	}
	writeConfig(t, dir, "both", "container: x\nurl: https://e.org/u.img.xz\nsha256: "+strings.Repeat("ab", 32)+"\ncmdline: x\n")
	if _, err := Load(dir, "both"); err == nil {
		t.Error("container with url: want error")
	}
	cache := t.TempDir()
	if _, err := Fetch(t.Context(), c, cache, io.Discard); err == nil || !strings.Contains(err.Error(), "podman save") {
		t.Errorf("no layout: err = %v", err)
	}
	os.MkdirAll(filepath.Join(cache, "oci", "rocky10-rpi"), 0o755)
	os.WriteFile(filepath.Join(cache, "oci", "rocky10-rpi", "index.json"), []byte("{}"), 0o644)
	if p, err := Fetch(t.Context(), c, cache, io.Discard); err != nil || p != filepath.Join(cache, "oci", "rocky10-rpi") {
		t.Errorf("Fetch = %q, %v", p, err)
	}
}
