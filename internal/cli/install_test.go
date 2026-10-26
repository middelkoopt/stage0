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
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/ulikunitz/xz"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/cpio"
	"github.com/middelkoopt/stage0/internal/netswitch"
	"github.com/middelkoopt/stage0/internal/reach"
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
// chunk "<rebooted>" waits until the switch has powered the node on twice;
// "<dhcp-pxe>" and "<dhcp-plain>" make the node ask stage0's DHCP for an
// address, as its bootloader and its installed system do.
type scripted struct {
	chunks []string
	sw     *fakeSwitch
	dhcp   func(pxe bool)
}

func (s *scripted) Read(p []byte) (int, error) {
	if len(s.chunks) > 0 && s.chunks[0] == "<rebooted>" && s.sw.ons() >= 2 {
		s.chunks = s.chunks[1:]
	}
	for len(s.chunks) > 0 && (s.chunks[0] == "<dhcp-pxe>" || s.chunks[0] == "<dhcp-plain>") {
		s.dhcp(s.chunks[0] == "<dhcp-pxe>")
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
	state  string
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
		"\ncmdline: console=serial0,115200 root=PARTUUID=abc rootwait\nconfig: [dtoverlay=disable-bt]\n"), 0o644)
	agent := filepath.Join(dir, "stage0")
	os.WriteFile(agent, []byte("AGENT"), 0o755)

	sw := &fakeSwitch{st: testStatus()}
	env := &installEnv{sw: sw, cache: filepath.Join(dir, "cache"), images: images, sum: sum}
	var dhcpAddr net.Addr
	var dhcpMu sync.Mutex
	// ask sends a DHCP discover and request for 10.0.3.100 from k53's MAC.
	ask := func(pxe bool) {
		var to net.Addr
		for deadline := time.Now().Add(5 * time.Second); to == nil && time.Now().Before(deadline); {
			dhcpMu.Lock()
			to = dhcpAddr // set once serve opens its sockets
			dhcpMu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		hw, _ := net.ParseMAC("d8:3a:dd:c5:c1:f3")
		var mods []dhcpv4.Modifier
		if pxe {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00000:UNDI:002001")))
		}
		d, _ := dhcpv4.NewDiscovery(hw, mods...)
		c.WriteTo(d.ToBytes(), to)
		r, _ := dhcpv4.New(append(mods, dhcpv4.WithHwAddr(hw), dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("10.0.3.100"))))...)
		c.WriteTo(r.ToBytes(), to)
		time.Sleep(50 * time.Millisecond) // let serve handle them
	}
	env.app = &App{
		OpenSwitch: func(config.Switch, config.SwitchSecret) (netswitch.Controller, error) { return sw, nil },
		OpenConsole: func(string, int) (io.ReadWriteCloser, error) {
			return &scripted{chunks: console, sw: sw, dhcp: ask}, nil
		},
		OpenNetwork: func(context.Context, config.Network) (Sockets, error) {
			d, _ := net.ListenPacket("udp4", "127.0.0.1:0")
			dhcpMu.Lock()
			dhcpAddr = d.LocalAddr()
			dhcpMu.Unlock()
			tf, _ := net.ListenPacket("udp4", "127.0.0.1:0")
			h, _ := net.Listen("tcp4", "127.0.0.1:0")
			return Sockets{DHCP: d, TFTP: tf, HTTP: h}, nil
		},
		CheckStatic: func(string) error { return nil },
		Executable:  func() (string, error) { return agent, nil },
		FindAddress: func(_ context.Context, ifname string, macs []net.HardwareAddr) ([]netip.Addr, error) {
			if ifname != "eth0.3" || len(macs) != 1 || macs[0].String() != "d8:3a:dd:c5:c1:f3" {
				t.Errorf("looked up %v on %s", macs, ifname)
			}
			return []netip.Addr{netip.MustParseAddr("fe80::5662:976b:2c19:7e38")}, nil
		},
		Reach: func(_ context.Context, addr string) reach.Result {
			return reach.Result{Address: addr, Ping: true, SSH: "SSH-2.0-OpenSSH_9.9"}
		},
	}
	env.state = filepath.Join(dir, "state")
	return env
}

func (e *installEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Chdir(t.TempDir()) // logs/ goes here
	return runApp(t, e.app, append([]string{"--state", e.state, "install", "k53", "--image", "test",
		"--images", e.images, "--cache", e.cache, "--timeout", "20s"}, args...)...)
}

func TestInstall(t *testing.T) {
	env := newInstallEnv(t, []string{
		"<dhcp-pxe>",
		"bootloader...\r\n",
		`STAGE0-REPORT {"model":"Raspberry Pi 5","disks":[{"name":"mmcblk0","bytes":128000000000}],` +
			`"interfaces":[{"name":"eth0","mac":"d8:3a:dd:c5:c1:f3","state":"up"}]}` + "\r\n",
		"STAGE0-PROGRESS 256 MB written, 5 s\r\n",
		`STAGE0-INSTALLED {"disk":"/dev/mmcblk0","bytes":3508535296,"seconds":104.9,"seeded":["cmdline.txt"]}` + "\r\n",
		"<rebooted>",
		"<dhcp-plain>",
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
		Address   string          `json:"address"`
		IPv4      string          `json:"ipv4"`
		Reach     struct {
			Address string `json:"address"`
			SSH     string `json:"ssh"`
		} `json:"reach"`
		Network string `json:"network"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Node != "k53" || res.Disk != "mmcblk0" || !strings.Contains(string(res.Installed), "3508535296") ||
		res.Ready != "login: " || res.Address != "fe80::5662:976b:2c19:7e38%eth0.3" ||
		res.IPv4 != "10.0.3.100" || res.Network != "pinet" || res.Reach.Address != "10.0.3.100" || res.Reach.SSH == "" {
		t.Errorf("result %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(env.state, "nodes", "k53.json")); err != nil || !strings.Contains(string(b), "d8:3a:dd:c5:c1:f3") {
		t.Errorf("install record: %s, %v", b, err)
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
		!strings.Contains(string(b), "initramfs stage0-initrd followkernel") || !strings.Contains(string(b), "enable_uart=1") ||
		!strings.Contains(string(b), "dtoverlay=disable-bt") {
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
	seed := map[string]string{}
	for {
		h, err := r.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(r)
		seed[h.Name] = string(b)
	}
	if c := seed["config.txt"]; !strings.HasPrefix(c, "arm_64bit=1\n") || !strings.HasSuffix(c, "[all]\ndtoverlay=disable-bt\n") ||
		strings.Contains(c, "stage0-initrd") {
		t.Errorf("seed config.txt = %q; want the image's plus its config lines, without the agent's", c)
	}
	h := cpio.Header{Name: "cmdline.txt"}
	body, err := []byte(seed["cmdline.txt"]), error(nil)
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

func TestGetAddress(t *testing.T) {
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "nodes"), 0o755)
	os.WriteFile(filepath.Join(state, "nodes", "k53.json"),
		[]byte(`{"node":"k53","report":{"interfaces":[{"name":"eth0","mac":"d8:3a:dd:c5:c1:f3"}]}}`), 0o644)
	app := &App{FindAddress: func(_ context.Context, ifname string, macs []net.HardwareAddr) ([]netip.Addr, error) {
		if ifname != "eth0.3" || len(macs) != 1 || macs[0].String() != "d8:3a:dd:c5:c1:f3" {
			t.Errorf("looked up %v on %s", macs, ifname)
		}
		return []netip.Addr{netip.MustParseAddr("fe80::1")}, nil
	}}
	out, err := runApp(t, app, "--state", state, "get", "address", "k53")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "fe80::1%eth0.3") || !strings.Contains(out, "ssh root@fe80::1%eth0.3") {
		t.Errorf("output %q", out)
	}
	if _, err := runApp(t, app, "--state", state, "get", "address", "k41"); err == nil || !strings.Contains(err.Error(), "install") {
		t.Errorf("node never installed: err = %v", err)
	}
}

func TestInstalledCmdlineTooLong(t *testing.T) {
	rsa := "ssh-rsa " + strings.Repeat("A", 540) + " someone@host" // an RSA 3072 key's size
	if _, err := installedCmdline("console=serial0,115200 root=PARTUUID=abc rootwait", "k52", []string{rsa}); err == nil ||
		!strings.Contains(err.Error(), "ed25519") {
		t.Errorf("err = %v, want a too-long error pointing at key size", err)
	}
	ed := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKZy0O24eEGhCIjhD/f/ARp+HXk7mQqud0uC6cWuIiup stage0@pilab"
	if _, err := installedCmdline("console=serial0,115200 root=PARTUUID=abc rootwait", "k52", []string{ed, ed, ed}); err != nil {
		t.Errorf("three ed25519 keys: %v", err)
	}
}

func TestServeAnswersInstalledNodes(t *testing.T) {
	// serve without --root: DHCP only, for nodes in the install records.
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "nodes"), 0o755)
	os.WriteFile(filepath.Join(state, "nodes", "k53.json"),
		[]byte(`{"node":"k53","network":"pinet","mac":"d8:3a:dd:c5:c1:f3","ipv4":"10.0.3.150"}`), 0o644)
	d, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	tf, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	h, _ := net.Listen("tcp4", "127.0.0.1:0")
	app := &App{OpenNetwork: func(context.Context, config.Network) (Sockets, error) {
		return Sockets{DHCP: d, TFTP: tf, HTTP: h}, nil
	}}
	done := make(chan string)
	go func() {
		out, err := runApp(t, app, "--state", state, "serve", "pinet", "--timeout", "1s")
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	time.Sleep(200 * time.Millisecond)
	c, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer c.Close()
	hw, _ := net.ParseMAC("d8:3a:dd:c5:c1:f3")
	disc, _ := dhcpv4.NewDiscovery(hw)
	c.WriteTo(disc.ToBytes(), d.LocalAddr())
	out := <-done
	if !strings.Contains(out, "dhcp: offer") || !strings.Contains(out, "ip=10.0.3.150") || !strings.Contains(out, "installed=1") {
		t.Errorf("serve log:\n%s", out)
	}
}

func TestCheck(t *testing.T) {
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "nodes"), 0o755)
	os.WriteFile(filepath.Join(state, "nodes", "k53.json"), []byte(`{"node":"k53","ipv4":"10.0.3.100"}`), 0o644)
	ok := true
	app := &App{Reach: func(_ context.Context, addr string) reach.Result {
		if addr != "10.0.3.100" {
			t.Errorf("checked %s", addr)
		}
		if !ok {
			return reach.Result{Address: addr, Err: "ping 10.0.3.100: no reply"}
		}
		return reach.Result{Address: addr, Ping: true, SSH: "SSH-2.0-OpenSSH_9.9"}
	}}
	out, err := runApp(t, app, "--state", state, "check", "k53")
	if err != nil || !strings.Contains(out, "answers ping and ssh (SSH-2.0-OpenSSH_9.9)") {
		t.Errorf("%q, %v", out, err)
	}
	ok = false
	if _, err := runApp(t, app, "--state", state, "check", "k53"); err == nil || !strings.Contains(err.Error(), "no reply") {
		t.Errorf("unreachable: err = %v", err)
	}
}
