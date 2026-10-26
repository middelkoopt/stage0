package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/cpio"
	"github.com/middelkoopt/stage0/internal/netswitch"
)

func gunzipFile(t *testing.T, path string) []byte {
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

// testDiskImage is an xz disk image: a GPT with the FAT12 test filesystem
// (holding config.txt) as partition 1. It returns the image and its SHA-256.
func testDiskImage(t *testing.T) ([]byte, string) {
	t.Helper()
	disk := gunzipFile(t, "../ptable/testdata/gpt.img.gz")
	copy(disk[1<<20:], gunzipFile(t, "../fat/testdata/fat12.img.gz"))
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

// scripted is a serial port that prints its chunks, then stays quiet. A
// chunk "<rebooted>" waits until the switch has powered the node on twice.
type scripted struct {
	chunks []string
	sw     *fakeSwitch
}

func (s *scripted) Read(p []byte) (int, error) {
	if len(s.chunks) > 0 && s.chunks[0] == "<rebooted>" && s.sw.ons() >= 2 {
		s.chunks = s.chunks[1:]
	}
	if len(s.chunks) == 0 || s.chunks[0] == "<rebooted>" {
		time.Sleep(5 * time.Millisecond)
		return 0, nil
	}
	n := copy(p, s.chunks[0])
	s.chunks = s.chunks[1:]
	return n, nil
}
func (s *scripted) Write(p []byte) (int, error) { return len(p), nil }
func (s *scripted) Close() error                { return nil }

type installEnv struct {
	app    *App
	sw     *fakeSwitch
	cache  string
	images string
	sum    string
}

func newInstallEnv(t *testing.T, console []string) *installEnv {
	t.Helper()
	img, sum := testDiskImage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(img) }))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	os.Mkdir(images, 0o755)
	os.WriteFile(filepath.Join(images, "test.yaml"), []byte("url: "+srv.URL+"/test.raw.xz\nsha256: "+sum+
		"\ncmdline: console=serial0,115200 root=PARTUUID=abc rootwait\n"), 0o644)
	agent := filepath.Join(dir, "stage0")
	os.WriteFile(agent, []byte("AGENT"), 0o755)

	sw := &fakeSwitch{st: testStatus()}
	env := &installEnv{sw: sw, cache: filepath.Join(dir, "cache"), images: images, sum: sum}
	env.app = &App{
		OpenSwitch:  func(config.Switch, config.SwitchSecret) (netswitch.Controller, error) { return sw, nil },
		OpenConsole: func(string, int) (io.ReadWriteCloser, error) { return &scripted{chunks: console, sw: sw}, nil },
		OpenNetwork: func(context.Context, config.Network) (Sockets, error) {
			d, _ := net.ListenPacket("udp4", "127.0.0.1:0")
			tf, _ := net.ListenPacket("udp4", "127.0.0.1:0")
			h, _ := net.Listen("tcp4", "127.0.0.1:0")
			return Sockets{DHCP: d, TFTP: tf, HTTP: h}, nil
		},
		CheckStatic: func(string) error { return nil },
		Executable:  func() (string, error) { return agent, nil },
	}
	return env
}

func (e *installEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Chdir(t.TempDir()) // logs/ goes here
	return runApp(t, e.app, append([]string{"install", "k53", "--image", "test",
		"--images", e.images, "--cache", e.cache, "--timeout", "20s"}, args...)...)
}

func TestInstall(t *testing.T) {
	env := newInstallEnv(t, []string{
		"bootloader...\r\n",
		`STAGE0-REPORT {"model":"Raspberry Pi 5","disks":[{"name":"mmcblk0","bytes":128000000000}]}` + "\r\n",
		"STAGE0-PROGRESS 256 MB written, 5 s\r\n",
		`STAGE0-INSTALLED {"disk":"/dev/mmcblk0","bytes":3508535296,"seconds":104.9,"seeded":["cmdline.txt"]}` + "\r\n",
		"<rebooted>",
		"Rocky Linux 10.2\r\nlocalhost login: ",
	})
	out, err := env.run(t, "-o", "json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var res struct {
		Node      string          `json:"node"`
		Disk      string          `json:"disk"`
		Installed json.RawMessage `json:"installed"`
		Ready     string          `json:"ready"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Node != "k53" || res.Disk != "mmcblk0" || !strings.Contains(string(res.Installed), "3508535296") || res.Ready != "login: " {
		t.Errorf("result %+v", res)
	}
	// Off and on to netboot, then off and on to boot from disk.
	if got := strings.Join(env.sw.writes, ","); got != "poe 2 false,poe 2 true,poe 2 false,poe 2 true" {
		t.Errorf("switch writes %s", got)
	}

	run := filepath.Join(env.cache, "run", "k53")
	cmdline, _ := os.ReadFile(filepath.Join(run, "cmdline.txt"))
	for _, want := range []string{"ip=dhcp", "stage0.disk=mmcblk0", "stage0.sha256=" + env.sum,
		"stage0.url=http://10.0.3.1/images/test.raw.xz", "stage0.seed=http://10.0.3.1/seed.cpio"} {
		if !strings.Contains(string(cmdline), want) {
			t.Errorf("cmdline.txt lacks %q: %s", want, cmdline)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(run, "config.txt")); !strings.HasPrefix(string(b), "arm_64bit=1\n") ||
		!strings.Contains(string(b), "initramfs stage0-initrd followkernel") {
		t.Errorf("config.txt = %q; want the image's, plus stage0's initramfs", b)
	}
	if _, err := os.Stat(filepath.Join(run, "stage0-initrd")); err != nil {
		t.Error(err)
	}
	f, err := os.Open(filepath.Join(run, "seed.cpio"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := cpio.NewReader(f)
	h, err := r.Next()
	body, _ := io.ReadAll(r)
	key := base64.StdEncoding.EncodeToString([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample stage0@test\n"))
	want := "console=serial0,115200 root=PARTUUID=abc rootwait systemd.hostname=k53" +
		" systemd.set_credential_binary=ssh.authorized_keys.root:" + key + "\n"
	if err != nil || h.Name != "cmdline.txt" || string(body) != want {
		t.Errorf("seed: %+v %v\n got %q\nwant %q", h, err, body, want)
	}
}

func TestInstallAgentError(t *testing.T) {
	env := newInstallEnv(t, []string{"STAGE0-ERROR install: payload checksum mismatch\r\n"})
	_, err := env.run(t)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v", err)
	}
	if got := strings.Join(env.sw.writes, ","); got != "poe 2 false,poe 2 true,poe 2 false" {
		t.Errorf("switch writes %s; want the node left off", got)
	}
}

func TestInstallWrongVLAN(t *testing.T) {
	env := newInstallEnv(t, nil)
	env.sw.st.Ports[1].PVID = 5 // k53 on piwulf
	_, err := env.run(t)
	if err == nil || !strings.Contains(err.Error(), "set port") {
		t.Errorf("err = %v", err)
	}
	if len(env.sw.writes) != 0 {
		t.Errorf("switch writes %v", env.sw.writes)
	}
}
