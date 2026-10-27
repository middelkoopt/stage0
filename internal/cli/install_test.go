package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/middelkoopt/stage0/internal/image"
	"github.com/middelkoopt/stage0/internal/netswitch"
	"github.com/middelkoopt/stage0/internal/oci/ocitest"
	"github.com/middelkoopt/stage0/internal/reach"
	"github.com/middelkoopt/stage0/internal/vswitch"
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
	http   net.Listener
	ask    func(pxe bool)
	askAs  func(mac, class, addr string)
	httpAt func() net.Addr
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
		"\ncmdline: console=serial0,115200 root=PARTUUID=abc rootwait\nconfig: [dtoverlay=disable-bt]\nbootFiles: {ssh: \"\"}\n"), 0o644)
	agent := filepath.Join(dir, "stage0")
	os.WriteFile(agent, []byte("AGENT"), 0o755)

	sw := &fakeSwitch{st: testStatus()}
	env := &installEnv{sw: sw, cache: filepath.Join(dir, "cache"), images: images, sum: sum}
	var dhcpAddr, httpAddr net.Addr
	var dhcpMu sync.Mutex
	// askAs sends a DHCP discover and request for addr from mac, with a
	// vendor class (or none).
	var askAs func(mac, class, addr string)
	// ask sends a DHCP discover and request for 10.0.3.100 from k53's MAC.
	ask := func(pxe bool) {
		class := ""
		if pxe {
			class = "PXEClient:Arch:00000:UNDI:002001"
		}
		askAs("d8:3a:dd:c5:c1:f3", class, "10.0.3.100")
	}
	askAs = func(macStr, class, addr string) {
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
		hw, _ := net.ParseMAC(macStr)
		var mods []dhcpv4.Modifier
		if class != "" {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(class)))
		}
		d, _ := dhcpv4.NewDiscovery(hw, mods...)
		c.WriteTo(d.ToBytes(), to)
		r, _ := dhcpv4.New(append(mods, dhcpv4.WithHwAddr(hw), dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP(addr))))...)
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
			defer func() {
				dhcpMu.Lock()
				httpAddr = env.http.Addr()
				dhcpMu.Unlock()
			}()
			tf, _ := net.ListenPacket("udp4", "127.0.0.1:0")
			h, _ := net.Listen("tcp4", "127.0.0.1:0")
			env.http = h
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
	env.ask = ask
	env.askAs = askAs
	env.httpAt = func() net.Addr {
		dhcpMu.Lock()
		defer dhcpMu.Unlock()
		return httpAddr
	}
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
	if v, ok := seed["ssh"]; !ok || v != "" {
		t.Errorf("seed lacks the empty ssh boot file: %v", seed)
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

func TestInstallSetPort(t *testing.T) {
	env := newInstallEnv(t, []string{"STAGE0-ERROR install: stop here\r\n"})
	env.sw.st.Ports[1].PVID, env.sw.st.Ports[1].Tagged = 5, []int{3, 23} // k53 on piwulf, pinet tagged
	_, err := env.run(t, "--set-port")
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("err = %v", err)
	}
	// PVID pinet(3), the old PVID piwulf(5) kept tagged; then off and on.
	if got := strings.Join(env.sw.writes, ","); !strings.HasPrefix(got, "vlans 2 3 [5 23],poe 2 false,poe 2 true") {
		t.Errorf("switch writes %s", got)
	}
}

func TestInstallWithoutConsole(t *testing.T) {
	// k52 has no console: the agent reports over HTTP, and the node is up
	// when its system takes its lease and answers ping and SSH.
	env := newInstallEnv(t, nil)
	env.app.OpenConsole = func(string, int) (io.ReadWriteCloser, error) {
		t.Error("opened a console for a node without one")
		return nil, errors.New("no console")
	}
	go func() {
		for env.sw.ons() < 1 {
			time.Sleep(10 * time.Millisecond)
		}
		env.ask(true) // the bootloader
		var to net.Addr
		for to == nil {
			time.Sleep(10 * time.Millisecond)
			to = env.httpAt()
		}
		post := func(kind, body string) {
			r, err := http.Post("http://"+to.String()+"/stage0/report/"+kind, "application/octet-stream", strings.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			r.Body.Close()
		}
		post("report", `{"model":"Pi 5","interfaces":[{"name":"eth0","mac":"d8:3a:dd:c5:c1:f3"}]}`)
		post("installed", `{"disk":"/dev/mmcblk0","bytes":3508535296,"seconds":100}`)
		for env.sw.ons() < 2 {
			time.Sleep(10 * time.Millisecond)
		}
		env.ask(false) // the installed system
	}()
	t.Chdir(t.TempDir())
	out, err := runApp(t, env.app, "--state", env.state, "install", "k52", "--image", "test", "--set-port",
		"--images", env.images, "--cache", env.cache, "--timeout", "20s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"agent running", "wrote 3.5 GB", "10.0.3.100 answers ping and ssh"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "on the console") {
		t.Errorf("mentions a console:\n%s", out)
	}
}

func TestInstallSilentConsole(t *testing.T) {
	// The console goes quiet after the agent (a Pi 4 without enable_uart):
	// the lease and ping and SSH still show the node is up.
	env := newInstallEnv(t, []string{
		"<dhcp-pxe>",
		`STAGE0-REPORT {"interfaces":[{"name":"eth0","mac":"d8:3a:dd:c5:c1:f3"}]}` + "\r\n",
		`STAGE0-INSTALLED {"disk":"/dev/mmcblk0","bytes":1}` + "\r\n",
		"<rebooted>",
		"<dhcp-plain>",
	})
	readyWaitSave := readyWait
	readyWait = 500 * time.Millisecond
	defer func() { readyWait = readyWaitSave }()
	out, err := env.run(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "no \"login: \" on the console") || !strings.Contains(out, "answers ping and ssh") {
		t.Errorf("output:\n%s", out)
	}
}

// fakeQEMU is a VM: it serves the console socket QEMU would and, booted
// with -kernel, plays the agent; booted from disk, the installed system.
type fakeQEMU struct{ ln net.Listener }

func (q *fakeQEMU) Stop() error { return q.ln.Close() }

func TestInstallVM(t *testing.T) {
	env := newInstallEnv(t, nil)
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	files := map[string][]byte{"/vmlinuz": []byte("KERNEL"), "/initrd.img": []byte("INITRD")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(files[r.URL.Path]) }))
	defer srv.Close()
	os.WriteFile(filepath.Join(env.images, "vm.yaml"), []byte(fmt.Sprintf(
		"url: %s/disk.qcow2\nsha256: %s\nnocloud: true\nkernel: {url: %s/vmlinuz, sha256: %s}\ninitrd: {url: %s/initrd.img, sha256: %s}\n",
		srv.URL, strings.Repeat("ab", 32), srv.URL, sum(files["/vmlinuz"]), srv.URL, sum(files["/initrd.img"]))), 0o644)
	// The disk image itself is never fetched by stage0 here: pre-place it.
	os.MkdirAll(env.cache, 0o755)
	img := []byte("QCOW")
	os.WriteFile(filepath.Join(env.images, "vm.yaml"), []byte(fmt.Sprintf(
		"url: %s/disk.qcow2\nsha256: %s\nnocloud: true\nkernel: {url: %s/vmlinuz, sha256: %s}\ninitrd: {url: %s/initrd.img, sha256: %s}\n",
		srv.URL, sum(img), srv.URL, sum(files["/vmlinuz"]), srv.URL, sum(files["/initrd.img"]))), 0o644)
	os.WriteFile(filepath.Join(env.cache, "disk.qcow2"), img, 0o644)

	fw := filepath.Join(t.TempDir(), "AAVMF_CODE.fd")
	os.WriteFile(fw, []byte("CODE"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(fw), "AAVMF_VARS.fd"), []byte("VARS"), 0o644)
	env.app.OpenTap = func(name string) (vswitch.Port, error) {
		if name != "stage0tap0" {
			t.Errorf("tap %q", name)
		}
		return &pipePort{in: make(chan []byte), out: make(chan []byte, 64)}, nil
	}
	env.app.OpenConsole = func(device string, baud int) (io.ReadWriteCloser, error) {
		return net.Dial("unix", strings.TrimPrefix(device, "unix:"))
	}
	env.app.FindAddress = func(_ context.Context, ifname string, macs []net.HardwareAddr) ([]netip.Addr, error) {
		if ifname != "stage0tap0" || len(macs) != 1 || macs[0].String() != "52:54:00:12:34:56" {
			t.Errorf("looked up %v on %s", macs, ifname)
		}
		return []netip.Addr{netip.MustParseAddr("fe80::5054:ff:fe12:3456")}, nil
	}
	var boots []string
	env.app.StartVM = func(args []string, _ io.Writer) (VMProcess, error) {
		joined := strings.Join(args, " ")
		agentBoot := strings.Contains(joined, "-kernel")
		boots = append(boots, map[bool]string{true: "agent", false: "disk"}[agentBoot])
		var sock string
		for _, f := range strings.Split(joined, ",") {
			if p, ok := strings.CutPrefix(f, "path="); ok && strings.HasSuffix(p, ".console") {
				sock = p
			}
		}
		if agentBoot && (!strings.Contains(joined, "stage0.disk=vda") || !strings.Contains(joined, "/initrd ")) {
			t.Errorf("agent boot args: %s", joined)
		}
		if !agentBoot && !strings.Contains(joined, "serial=ds=nocloud;s=http://10.0.9.1/nocloud/vm1/") {
			t.Errorf("disk boot args: %s", joined)
		}
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return nil, err
		}
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			if agentBoot {
				env.askAs("52:54:00:12:34:56", "stage0-agent", "10.0.9.100")
				io.WriteString(c, `STAGE0-REPORT {"interfaces":[{"name":"eth0","mac":"52:54:00:12:34:56"}]}`+"\r\n")
				io.WriteString(c, `STAGE0-INSTALLED {"disk":"/dev/vda","bytes":10737418240,"seconds":24}`+"\r\nmore\r\n")
			} else {
				io.WriteString(c, "Rocky Linux 10.2\r\nvm1 login: ")
				env.askAs("52:54:00:12:34:56", "", "10.0.9.100")
			}
			time.Sleep(time.Hour) // until Stop closes the listener (the test ends)
		}()
		return &fakeQEMU{ln: ln}, nil
	}
	t.Chdir(t.TempDir())
	out, err := runApp(t, env.app, "--state", env.state, "install", "vm1", "--image", "vm",
		"--images", env.images, "--cache", env.cache, "--timeout", "20s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(boots, ",") != "agent,disk" {
		t.Errorf("boots %v", boots)
	}
	for _, want := range []string{"vm1 (VM): powered on", "wrote 10.7 GB", "up (\"login: \"", "10.0.9.100 answers ping and ssh"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	ud, _ := os.ReadFile(filepath.Join(env.cache, "run", "vm1", "nocloud", "vm1", "user-data"))
	if !strings.Contains(string(ud), "hostname: vm1") || !strings.Contains(string(ud), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample stage0@test") {
		t.Errorf("user-data:\n%s", ud)
	}
	if len(env.sw.writes) != 0 {
		t.Errorf("a VM install touched the switch: %v", env.sw.writes)
	}
}

func TestInstallKeep(t *testing.T) {
	// --keep: after the install, keep serving until the time limit, then
	// end without an error.
	env := newInstallEnv(t, []string{
		"<dhcp-pxe>",
		`STAGE0-REPORT {"interfaces":[{"name":"eth0","mac":"d8:3a:dd:c5:c1:f3"}]}` + "\r\n",
		`STAGE0-INSTALLED {"disk":"/dev/mmcblk0","bytes":1}` + "\r\n",
		"<rebooted>",
		"<dhcp-plain>",
		"k53 login: ",
	})
	start := time.Now()
	t.Chdir(t.TempDir())
	out, err := runApp(t, env.app, "--state", env.state, "install", "k53", "--image", "test", "--keep",
		"--images", env.images, "--cache", env.cache, "--timeout", "3s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "keeping") || time.Since(start) < 2*time.Second {
		t.Errorf("did not keep serving (%s):\n%s", time.Since(start), out)
	}
}

func TestInstallIPv6Staging(t *testing.T) {
	// The test image has no initramfs8, which --ipv6 needs (the kernel's
	// ipv6 module is in it): staging stops there, with what came before.
	env := newInstallEnv(t, nil)
	if _, err := env.run(t, "--ipv6"); err == nil || !strings.Contains(err.Error(), "needs its initramfs8") {
		t.Fatalf("err = %v", err)
	}
	if cfg, _ := os.ReadFile(filepath.Join(env.cache, "run", "k53", "config.txt")); !strings.Contains(string(cfg), "kernel=kernel8.img") {
		t.Errorf("config.txt: %s", cfg)
	}
	cmdline, _ := os.ReadFile(filepath.Join(env.cache, "run", "k53", "cmdline.txt"))
	if !strings.Contains(string(cmdline), "stage0.url=http://[fd00:10:3::1]/images/test.raw.xz") ||
		!strings.Contains(string(cmdline), "stage0.report=http://[fd00:10:3::1]/stage0/report") ||
		strings.Contains(string(cmdline), "ip=dhcp") {
		t.Errorf("cmdline.txt: %s", cmdline)
	}
}

func TestStageBootDir(t *testing.T) {
	// Ubuntu: config.txt at the top sets os_prefix=current/, so the
	// firmware reads cmdline.txt and initramfs from current/.
	dir := t.TempDir()
	boot := filepath.Join(dir, "boot")
	os.MkdirAll(filepath.Join(boot, "current"), 0o755)
	os.WriteFile(filepath.Join(boot, "config.txt"), []byte("[all]\nos_prefix=current/\n"), 0o644)
	os.WriteFile(filepath.Join(boot, "current", "initrd.img"), []byte("DISTRO"), 0o644)
	os.WriteFile(filepath.Join(boot, "user-data"), []byte("#cloud-config\n"), 0o644)
	agent := filepath.Join(dir, "stage0")
	os.WriteFile(agent, []byte("AGENT"), 0o755)
	a := &App{Executable: func() (string, error) { return agent, nil }, CheckStatic: func(string) error { return nil }}
	img := image.Config{Name: "u", URL: "https://e.org/u.img.xz", SHA256: "ab", BootDir: "current", Initramfs: "current/initrd.img"}
	os.WriteFile(filepath.Join(dir, "u.img.xz"), []byte("IMG"), 0o644)
	run := filepath.Join(dir, "run")
	if err := a.stage(run, filepath.Join(dir, "u.img.xz"), img, "root=LABEL=writable", "k52", "mmcblk0", "10.0.3.1", boot); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(run, "current", "cmdline.txt")); err != nil || !strings.Contains(string(b), "stage0.disk=mmcblk0") {
		t.Errorf("current/cmdline.txt: %q %v", b, err)
	}
	// Padded to a 4-byte boundary, then the agent's gzip-compressed cpio.
	if b, err := os.ReadFile(filepath.Join(run, "current", initrdName)); err != nil || !bytes.HasPrefix(b, []byte("DISTRO\x00\x00\x1f\x8b")) {
		t.Errorf("current/%s: %d bytes %v", initrdName, len(b), err)
	}
	if _, err := os.Stat(filepath.Join(run, "cmdline.txt")); err == nil {
		t.Error("cmdline.txt at the top too")
	}
}

func TestStageContainer(t *testing.T) {
	dir := t.TempDir()
	layout := filepath.Join(dir, "oci", "rocky")
	digest := ocitest.Write(t, layout, map[string]string{
		"pi.provision.partition": `[{"name":"piboot","size":"512MiB","type":"vfat","path":"/boot/efi"},{"name":"piroot","type":"ext4","path":"/"}]`,
		"pi.provision.installer": `{"kernel":"/boot/efi/kernel8.img","initramfs":"/boot/installer.img"}`,
	}, map[string][]byte{
		"boot/efi": nil, "boot/efi/config.txt": []byte("arm_64bit=1\n"), "boot/efi/kernel8.img": []byte("KERNEL"),
		"boot/installer.img": []byte("DRACUT"), "etc/os-release": []byte("x"),
	})
	agent := filepath.Join(dir, "stage0")
	os.WriteFile(agent, []byte("AGENT"), 0o755)
	a := &App{Executable: func() (string, error) { return agent, nil }, CheckStatic: func(string) error { return nil }}
	img := image.Config{Name: "rocky-ct", Container: "rocky", Config: []string{"dtoverlay=disable-bt"}}
	run := filepath.Join(dir, "run")
	if err := a.stageContainer(run, layout, img, "root=PARTLABEL=piroot", "k52", "mmcblk0", "10.0.3.1", filepath.Join(dir, "boot")); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(run, "config.txt"))
	for _, l := range []string{"kernel=kernel8.img", "initramfs " + initrdName + " followkernel", "dtoverlay=disable-bt"} {
		if !strings.Contains(string(cfg), l+"\n") {
			t.Errorf("config.txt lacks %q:\n%s", l, cfg)
		}
	}
	cmdline, _ := os.ReadFile(filepath.Join(run, "cmdline.txt"))
	for _, f := range []string{"rd.neednet=1", "rd.net.dhcp.vendor-class=stage0-agent", "stage0.disk=mmcblk0",
		"stage0.sha256=" + strings.TrimPrefix(digest, "sha256:"), "stage0.oci=http://10.0.3.1/oci"} {
		if !strings.Contains(string(cmdline), f) {
			t.Errorf("cmdline.txt lacks %q: %s", f, cmdline)
		}
	}
	// The installer, padded to 4 bytes, then the agent and hook, gzip'd.
	if b, _ := os.ReadFile(filepath.Join(run, initrdName)); !bytes.HasPrefix(b, []byte("DRACUT\x00\x00\x1f\x8b")) {
		t.Errorf("%s starts %q", initrdName, b[:min(len(b), 12)])
	}
	if b, err := os.ReadFile(filepath.Join(run, "oci", "index.json")); err != nil || len(b) == 0 {
		t.Errorf("layout not served: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run, "seed.cpio")); err != nil {
		t.Error(err)
	}
}
