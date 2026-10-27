package deb

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// build makes a .deb: debian-binary, control.tar.<comp>, data.tar.<comp>
// holding files.
func build(t *testing.T, comp string, files map[string]string) []byte {
	t.Helper()
	tarball := func(files map[string]string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
			io.WriteString(tw, body)
		}
		tw.Close()
		var out bytes.Buffer
		var w io.WriteCloser
		switch comp {
		case "xz":
			w, _ = xz.NewWriter(&out)
		case "zst":
			w, _ = zstd.NewWriter(&out)
		}
		w.Write(buf.Bytes())
		w.Close()
		return out.Bytes()
	}
	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&ar, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(body))
		ar.Write(body)
		if len(body)%2 == 1 {
			ar.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar."+comp, tarball(map[string]string{"./control": "Package: x\n"}))
	member("data.tar."+comp, tarball(files))
	return ar.Bytes()
}

func TestFile(t *testing.T) {
	for _, comp := range []string{"xz", "zst"} {
		d := build(t, comp, map[string]string{
			"./usr/lib/systemd/boot/efi/linuxaa64.efi.stub": "STUB",
			"./usr/share/doc/x/copyright":                   "odd length!",
		})
		got, err := File(bytes.NewReader(d), "/usr/lib/systemd/boot/efi/linuxaa64.efi.stub")
		if err != nil || string(got) != "STUB" {
			t.Errorf("%s: %q, %v", comp, got, err)
		}
		if _, err := File(bytes.NewReader(d), "/usr/missing"); err == nil || !strings.Contains(err.Error(), "not in") {
			t.Errorf("%s: missing file: err = %v", comp, err)
		}
	}
	if _, err := File(strings.NewReader("not a deb"), "/x"); err == nil {
		t.Error("not a deb: want error")
	}
}
