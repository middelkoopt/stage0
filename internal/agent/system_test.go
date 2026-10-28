package agent

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/middelkoopt/stage0/internal/osconfig"
)

func TestConfigureSystem(t *testing.T) {
	findBootMAC = func(string) (string, error) { return "d8:3a:dd:c5:c1:f3", nil }
	defer func() { findBootMAC = bootMAC }()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "machine-id"), []byte("42a6c8e7a7b3441fbc52d097d8a2b138\n"), 0o444)
	os.WriteFile(filepath.Join(root, "etc", "hostname"), []byte("localhost\n"), 0o600)
	got, err := configureSystem(io.Discard, root, osconfig.System{Hostname: "k52", Keys: []string{"ssh-ed25519 AAAA a@b"},
		Network: osconfig.NetworkManager}, "http://10.0.3.1/oci")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/etc/hostname", "/root/.ssh/authorized_keys", "/etc/NetworkManager/system-connections/stage0-eth0.nmconnection", "/etc/machine-id"} {
		if !slices.Contains(got, p) {
			t.Errorf("%s not reported: %v", p, got)
		}
	}
	for p, mode := range map[string]os.FileMode{"etc/hostname": 0o644, "root/.ssh/authorized_keys": 0o600,
		"etc/NetworkManager/system-connections/stage0-eth0.nmconnection": 0o600} {
		if fi, err := os.Stat(filepath.Join(root, p)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want mode %v", p, fi, err, mode)
		}
	}
	if fi, _ := os.Stat(filepath.Join(root, "root", ".ssh")); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("/root/.ssh: %v", fi)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc", "machine-id")); len(b) != 0 {
		t.Errorf("machine-id kept: %q", b)
	}
}
