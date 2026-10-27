package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/middelkoopt/stage0/internal/oci/ocitest"
)

func layout(t *testing.T, dir string, labels map[string]string, layers ...map[string][]byte) {
	t.Helper()
	ocitest.Write(t, dir, labels, layers...)
}

func TestOpenLabels(t *testing.T) {
	dir := t.TempDir()
	layout(t, dir, map[string]string{"pi.provision.installer": `{"kernel":"/boot/k"}`}, map[string][]byte{"etc/os-release": []byte("x")})
	img, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if img.Labels["pi.provision.installer"] != `{"kernel":"/boot/k"}` || img.Architecture != "arm64" || len(img.layers) != 1 {
		t.Errorf("image %+v", img)
	}
}

func TestOpenNotLayout(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("empty directory: want error")
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	layout(t, dir, nil,
		map[string][]byte{"boot": nil, "boot/kernel8.img": []byte("old"), "boot/gone": []byte("x"), "etc/motd": []byte("m")},
		map[string][]byte{"boot/kernel8.img": []byte("new"), "boot/initrd.img": []byte("initrd"), "boot/.wh.gone": []byte{}},
	)
	img, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := img.Files("/boot/kernel8.img", "boot/initrd.img")
	if err != nil {
		t.Fatal(err)
	}
	if string(got["/boot/kernel8.img"]) != "new" || string(got["boot/initrd.img"]) != "initrd" {
		t.Errorf("files %q", got)
	}
	if _, err := img.Files("/boot/gone"); err == nil {
		t.Error("a whited-out file: want error")
	}
	if _, err := img.Files("/boot/missing"); err == nil {
		t.Error("a missing file: want error")
	}
}

func TestFilesBadDigest(t *testing.T) {
	dir := t.TempDir()
	layout(t, dir, nil, map[string][]byte{"boot/k": []byte("k")})
	img, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, filepath.FromSlash(blobPath(img.layers[0].Digest)))
	b, _ := os.ReadFile(p)
	b[len(b)-1] ^= 1
	os.WriteFile(p, b, 0o644)
	if _, err := img.Files("/boot/k"); err == nil {
		t.Error("a corrupted layer: want error")
	}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	layout(t, dir, nil,
		map[string][]byte{"boot/efi": nil, "boot/efi/start4.elf": []byte("s"), "boot/efi/overlays": nil,
			"boot/efi/overlays/a.dtbo": []byte("a"), "boot/efi/gone": []byte("g"), "etc/motd": []byte("m")},
		map[string][]byte{"boot/efi/start4.elf": []byte("s2"), "boot/efi/.wh.gone": []byte{},
			"boot/efi/overlays/.wh..wh..opq": []byte{}, "boot/efi/overlays/b.dtbo": []byte("b"),
			// In this layer before the opaque marker (entries are sorted):
			// it stays, as the marker hides only lower layers.
			"boot/efi/overlays/-x.dtbo": []byte("x")},
	)
	img, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := img.Extract("/boot/efi", dst); err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dst, p)
			got = append(got, rel+"="+string(b))
		}
		return nil
	})
	want := []string{"overlays/-x.dtbo=x", "overlays/b.dtbo=b", "start4.elf=s2"}
	if !slices.Equal(got, want) {
		t.Errorf("extracted %q, want %q", got, want)
	}
}

func TestOpenURL(t *testing.T) {
	dir := t.TempDir()
	layout(t, dir, map[string]string{"a": "b"}, map[string][]byte{"boot/k": []byte("k")})
	local, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	img, err := OpenURL(srv.URL+"/", local.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := img.Files("/boot/k"); err != nil || string(got["/boot/k"]) != "k" || img.Labels["a"] != "b" {
		t.Errorf("files %q, labels %v, err %v", got, img.Labels, err)
	}
	if _, err := OpenURL(srv.URL, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Error("unknown manifest digest: want error")
	}
	// A manifest that doesn't match its digest is refused.
	other := t.TempDir()
	layout(t, other, map[string]string{"a": "c"}, map[string][]byte{"boot/k": []byte("k")})
	o, _ := Open(other)
	mb, _ := os.ReadFile(filepath.Join(other, "blobs", "sha256", o.Digest[len("sha256:"):]))
	os.WriteFile(filepath.Join(dir, "blobs", "sha256", local.Digest[len("sha256:"):]), mb, 0o644)
	if _, err := OpenURL(srv.URL, local.Digest); err == nil {
		t.Error("a manifest with the wrong digest: want error")
	}
}

func TestUnpack(t *testing.T) {
	dir := t.TempDir()
	layout(t, dir, nil,
		map[string][]byte{"usr": nil, "usr/bin": nil, "usr/bin/ping": []byte("PING"), "etc": nil,
			"etc/gone": []byte("g"), "etc/old.d": nil, "etc/old.d/a": []byte("a")},
		map[string][]byte{"etc/.wh.gone": {}, "etc/old.d/.wh..wh..opq": {}, "etc/hostname": []byte("h")},
	)
	// A third layer, written by hand: a symlink, a hard link, a file
	// capability (xattr) and modes.
	img, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	n, err := img.Unpack(dst)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("PING")+len("g")+len("a")+len("h")) {
		t.Errorf("Unpack wrote %d bytes", n)
	}
	for p, want := range map[string]string{"usr/bin/ping": "PING", "etc/hostname": "h"} {
		if b, err := os.ReadFile(filepath.Join(dst, p)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", p, b, err)
		}
	}
	for _, p := range []string{"etc/gone", "etc/old.d/a"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s survived its whiteout", p)
		}
	}
}

func TestUnpackEntries(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, h := range []*tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/ping", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 4, ModTime: mtime,
			PAXRecords: map[string]string{"SCHILY.xattr.user.stage0": "cap"}},
		{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin"},
		{Name: "usr/bin/ping2", Typeflag: tar.TypeLink, Linkname: "usr/bin/ping"},
		{Name: "../escape", Typeflag: tar.TypeReg, Size: 0},
	} {
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte("PING"))
		}
	}
	tw.Close()
	if err := unpackTar(tar.NewReader(&buf), mustRoot(t, dir), dir, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(filepath.Join(dir, "bin")); err != nil || l != "usr/bin" {
		t.Errorf("bin -> %q, %v", l, err)
	}
	a, _ := os.Stat(filepath.Join(dir, "usr/bin/ping"))
	b, _ := os.Stat(filepath.Join(dir, "usr/bin/ping2"))
	if !os.SameFile(a, b) {
		t.Error("ping2 is not a hard link to ping")
	}
	if a.Mode()&os.ModeSetuid == 0 || a.Mode().Perm() != 0o755 || !a.ModTime().Equal(mtime) {
		t.Errorf("ping mode %v, mtime %v", a.Mode(), a.ModTime())
	}
	// user.* xattrs need a filesystem that keeps them (tmpfs does).
	v := make([]byte, 16)
	if n, err := unix.Getxattr(filepath.Join(dir, "usr/bin/ping"), "user.stage0", v); err != nil {
		t.Logf("xattr not checked: %v", err)
	} else if string(v[:n]) != "cap" {
		t.Errorf("xattr = %q", v[:n])
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Error("../escape was written outside the root")
	}
}

func mustRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// unpackTar unpacks one tar stream as a layer.
func unpackTar(tr *tar.Reader, root *os.Root, _ string, written map[string]bool) error {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := unpackEntry(root, written, path.Clean(strings.TrimPrefix(h.Name, "/")), h, tr); err != nil {
			return err
		}
	}
}
