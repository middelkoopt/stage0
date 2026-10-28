package initrd

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/middelkoopt/stage0/internal/cpio"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func entries(t *testing.T, b []byte) map[string]string {
	t.Helper()
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r := cpio.NewReader(z)
	got := map[string]string{}
	for {
		h, err := r.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		got[h.Name] = h.Mode.String() + " " + string(data)
	}
}

func TestBuild(t *testing.T) {
	var buf bytes.Buffer
	s, err := Build(&buf, write(t, "agent", "AGENT"), write(t, "payload.bin", "PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	got := entries(t, buf.Bytes())
	want := map[string]string{
		"dev":         "drwxr-xr-x ",
		"dev/console": "Dcrw------- ",
		"proc":        "drwxr-xr-x ",
		"sys":         "drwxr-xr-x ",
		"run":         "drwxr-xr-x ",
		"tmp":         "drwxrwxrwx ",
		"init":        "-rwxr-xr-x AGENT",
		"payload":     "-rw-r--r-- PAYLOAD",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("entries %v", got)
	}
	if s.AgentBytes != 5 || s.PayloadBytes != 7 || s.Bytes != int64(buf.Len()) {
		t.Errorf("summary %+v (archive %d bytes)", s, buf.Len())
	}
}

func TestBuildNoPayload(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Build(&buf, write(t, "agent", "AGENT"), ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := entries(t, buf.Bytes())["payload"]; ok {
		t.Error("payload entry without a payload")
	}
}

func TestCheckStatic(t *testing.T) {
	if err := CheckStatic(write(t, "script", "#!/bin/sh\n")); err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Errorf("non-ELF: err = %v", err)
	}
	for _, dyn := range []string{"/bin/sh", "/usr/bin/env"} {
		if _, err := os.Stat(dyn); err != nil {
			continue
		}
		if err := CheckStatic(dyn); err == nil || !strings.Contains(err.Error(), "dynamically linked") {
			t.Errorf("%s: err = %v, want dynamically linked", dyn, err)
		}
		return
	}
	t.Skip("no dynamic binary to check against")
}

func TestBuildSeed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cmdline.txt"), []byte("console=serial0,115200"), 0o644)
	os.MkdirAll(filepath.Join(dir, "user"), 0o755)
	os.WriteFile(filepath.Join(dir, "user", "user-data"), []byte("#cloud-config\n"), 0o644)
	var buf bytes.Buffer
	n, err := BuildSeed(&buf, dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d files, want 2", n)
	}
	r := cpio.NewReader(&buf)
	var names []string
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if got := strings.Join(names, " "); got != "cmdline.txt user user/user-data" {
		t.Errorf("entries %q", got)
	}
}

func TestBuildSeedRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	os.Symlink("/etc/passwd", filepath.Join(dir, "link"))
	if _, err := BuildSeed(io.Discard, dir); err == nil {
		t.Error("symlink accepted")
	}
}

func TestBuildHook(t *testing.T) {
	var buf bytes.Buffer
	if err := BuildHook(&buf, write(t, "agent", "AGENT")); err != nil {
		t.Fatal(err)
	}
	got := entries(t, buf.Bytes())
	if got["usr/bin/stage0"] != "-rwxr-xr-x AGENT" {
		t.Errorf("agent %q", got["usr/bin/stage0"])
	}
	// The hook waits for the installer's Ignition, when it partitions.
	if d := got["etc/systemd/system/dracut-pre-mount.service.d/90-stage0.conf"]; !strings.Contains(d, "After=ignition-disks.service") {
		t.Errorf("drop-in %q", d)
	}
	// A networkd installer asks DHCP as the agent.
	if d := got["etc/systemd/network/zzzz-dracut-default.network.d/90-stage0.conf"]; !strings.Contains(d, "VendorClassIdentifier=stage0-agent") {
		t.Errorf("networkd drop-in %q", d)
	}
	hook := got["var/lib/dracut/hooks/pre-mount/90-stage0.sh"]
	if !strings.HasPrefix(hook, "-rwxr-xr-x #!/bin/sh\n") || !strings.Contains(hook, "/usr/bin/stage0 agent") {
		t.Errorf("hook %q", hook)
	}
}

func TestAppend(t *testing.T) {
	for _, base := range []string{"", "a", "abcd", "abcde"} {
		var buf bytes.Buffer
		err := Append(&buf, strings.NewReader(base), func(w io.Writer) error {
			_, err := io.WriteString(w, "NEXT")
			return err
		})
		want := base + strings.Repeat("\x00", (4-len(base)%4)%4) + "NEXT"
		if err != nil || buf.String() != want {
			t.Errorf("base %q: %q, %v; want %q", base, buf.String(), err, want)
		}
	}
}
