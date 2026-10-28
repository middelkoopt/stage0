package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/middelkoopt/stage0/internal/container"
)

var testSpec = container.Spec{Partitions: []container.Partition{
	{Name: "piboot", Type: "vfat", Path: "/boot/efi", Bytes: 512 << 20},
	{Name: "piroot", Type: "ext4", Path: "/"},
}}

func TestSgdiskArgs(t *testing.T) {
	got := sgdiskArgs(testSpec, "/dev/mmcblk0")
	want := []string{"--zap-all", "--clear",
		"--new=1:0:+524288K", "--typecode=1:0700", "--change-name=1:piboot",
		"--new=2:0:0", "--typecode=2:8300", "--change-name=2:piroot",
		"/dev/mmcblk0"}
	if !slices.Equal(got, want) {
		t.Errorf("sgdisk %q\nwant %q", got, want)
	}
}

func TestMkfsArgs(t *testing.T) {
	if cmd, args := mkfsArgs(testSpec.Partitions[0], "/dev/mmcblk0p1"); cmd != "mkfs.vfat" || !slices.Equal(args, []string{"-F", "32", "-n", "PIBOOT", "/dev/mmcblk0p1"}) {
		t.Errorf("vfat: %s %q", cmd, args)
	}
	if cmd, args := mkfsArgs(testSpec.Partitions[1], "/dev/mmcblk0p2"); cmd != "mkfs.ext4" || !slices.Equal(args, []string{"-F", "-q", "-L", "piroot", "/dev/mmcblk0p2"}) {
		t.Errorf("ext4: %s %q", cmd, args)
	}
}

func TestFstab(t *testing.T) {
	want := "PARTLABEL=piroot / ext4 defaults 0 1\nPARTLABEL=piboot /boot/efi vfat defaults 0 2\n"
	if got := fstab(testSpec); got != want {
		t.Errorf("fstab %q\nwant %q", got, want)
	}
}

func TestSetupRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "resolv.conf"), nil, 0o644) // a container's empty one
	labels := map[string]string{
		"pi.provision.hosts":  `["127.0.0.1 localhost", "::1       localhost"]`,
		"pi.provision.resolv": `"../run/NetworkManager/no-stub-resolv.conf"`,
	}
	if err := setupRoot(root, testSpec, labels); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc", "hosts")); string(b) != "127.0.0.1 localhost\n::1       localhost\n" {
		t.Errorf("hosts %q", b)
	}
	if l, err := os.Readlink(filepath.Join(root, "etc", "resolv.conf")); err != nil || l != "../run/NetworkManager/no-stub-resolv.conf" {
		t.Errorf("resolv.conf -> %q, %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc", "fstab")); string(b) != fstab(testSpec) {
		t.Errorf("fstab %q", b)
	}
	if err := setupRoot(root, testSpec, map[string]string{"pi.provision.hosts": "not json"}); err == nil {
		t.Error("bad hosts label: want error")
	}
}

func TestClockEpoch(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755)
	at := time.Date(2026, 10, 27, 16, 30, 0, 0, time.UTC)
	if err := touchClockEpoch(root, at); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "usr/lib/clock-epoch"))
	if err != nil || !fi.ModTime().Equal(at) {
		t.Errorf("clock-epoch %v, %v", fi, err)
	}
}

func TestServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	got, err := serverTime(srv.URL + "/oci")
	if err != nil || time.Since(got).Abs() > 5*time.Second {
		t.Errorf("serverTime = %v, %v", got, err)
	}
}

func TestFetchLayout(t *testing.T) {
	good := `{"partitions":[{"name":"b","size":"256MiB","type":"vfat","path":"/boot/efi"},{"name":"r","type":"ext4","path":"/"}],"installer":{"kernel":"/k","initramfs":"/i"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad.json" {
			w.Write([]byte(`{"partitions":[{"name":"r","type":"ext4","path":"/"}],"installer":{"kernel":"/k","initramfs":"/i"}}`))
			return
		}
		w.Write([]byte(good))
	}))
	defer srv.Close()
	s, err := fetchLayout(srv.URL + "/layout.json")
	if err != nil || s.Partitions[0].Bytes != 256<<20 {
		t.Errorf("layout %+v, %v", s, err)
	}
	if _, err := fetchLayout(srv.URL + "/bad.json"); err == nil {
		t.Error("a root-only layout: want error")
	}
}

func TestMountOrder(t *testing.T) {
	spec := container.Spec{Partitions: []container.Partition{
		{Name: "boot", Type: "vfat", Path: "/boot/efi"},
		{Name: "log", Type: "ext4", Path: "/var/log"},
		{Name: "var", Type: "ext4", Path: "/var"},
		{Name: "root", Type: "ext4", Path: "/"},
	}}
	if got := mountOrder(spec); !slices.Equal(got, []int{2, 1}) {
		t.Errorf("mount order %v, want /var then /var/log", got)
	}
}

func TestWriteNoCloud(t *testing.T) {
	root := t.TempDir()
	ci := &container.CloudInit{UserData: "#cloud-config\n", MetaData: "instance-id: k52-1\n"}
	if err := writeNoCloud(root, ci); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "var/lib/cloud/seed/nocloud")
	for name, want := range map[string]string{"user-data": "#cloud-config\n", "meta-data": "instance-id: k52-1\n"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		fi, _ := os.Stat(filepath.Join(dir, name))
		if err != nil || string(b) != want || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s = %q (%v), %v", name, b, fi.Mode(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "network-config")); err == nil {
		t.Error("network-config written without one")
	}
}

func TestSELinux(t *testing.T) {
	root := t.TempDir()
	if _, ok := selinuxType(root); ok || selinuxOn(root) {
		t.Error("no config: want no SELinux")
	}
	os.MkdirAll(filepath.Join(root, "etc/selinux/targeted/contexts/files"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/selinux/config"), []byte("# x\nSELINUX=enforcing\nSELINUXTYPE=targeted\n"), 0o644)
	if _, ok := selinuxType(root); ok {
		t.Error("no file_contexts: want no policy")
	}
	os.WriteFile(filepath.Join(root, "etc/selinux/targeted/contexts/files/file_contexts"), nil, 0o644)
	if typ, ok := selinuxType(root); !ok || typ != "targeted" || !selinuxOn(root) {
		t.Errorf("policy: %q %v on=%v", typ, ok, selinuxOn(root))
	}
	os.WriteFile(filepath.Join(root, "etc/selinux/config"), []byte("SELINUX=disabled\nSELINUXTYPE=targeted\n"), 0o644)
	if selinuxOn(root) {
		t.Error("disabled: want off")
	}
}
