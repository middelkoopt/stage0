package cpio

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	steps := []error{
		w.Dir("dev", 0o755),
		w.CharDev("dev/console", 0o600, 5, 1),
		w.File("init", 0o755, 5, strings.NewReader("hello")),
		w.File("empty", 0o644, 0, strings.NewReader("")),
		w.File("odd/name.txt", 0o644, 3, strings.NewReader("abc")),
		w.Close(),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if buf.Len()%4 != 0 {
		t.Errorf("archive is %d bytes, not 4-byte aligned", buf.Len())
	}

	type entry struct {
		name         string
		mode         fs.FileMode
		major, minor uint32
		data         string
	}
	var got []entry
	r := NewReader(&buf)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, entry{h.Name, h.Mode, h.RdevMajor, h.RdevMinor, string(data)})
	}
	want := []entry{
		{"dev", fs.ModeDir | 0o755, 0, 0, ""},
		{"dev/console", fs.ModeDevice | fs.ModeCharDevice | 0o600, 5, 1, ""},
		{"init", 0o755, 0, 0, "hello"},
		{"empty", 0o644, 0, 0, ""},
		{"odd/name.txt", 0o644, 0, 0, "abc"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHeaderBytes(t *testing.T) {
	// The kernel reads newc: "070701", 13 eight-digit hex fields, the name
	// with its NUL, padding to 4, then the data padded to 4.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.File("a", 0o644, 2, strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	b := buf.String()
	if !strings.HasPrefix(b, "070701") {
		t.Fatalf("magic: %q", b[:6])
	}
	field := func(i int) string { return b[6+8*i : 14+8*i] }
	if field(1) != "000081a4" || field(4) != "00000001" || field(6) != "00000002" || field(11) != "00000002" {
		t.Errorf("mode %s nlink %s size %s namesize %s", field(1), field(4), field(6), field(11))
	}
	// 110-byte header + "a\0" = 112 (aligned), then "hi" padded to 4.
	if len(b) != 116 || b[110:112] != "a\x00" || b[112:116] != "hi\x00\x00" {
		t.Errorf("layout %q", b[110:])
	}
}

func TestFileSizeMismatch(t *testing.T) {
	w := NewWriter(io.Discard)
	if err := w.File("short", 0o644, 10, strings.NewReader("abc")); err == nil {
		t.Error("reader shorter than size: want error")
	}
}

func TestReaderRejectsGarbage(t *testing.T) {
	if _, err := NewReader(strings.NewReader("070707 old binary-ish format")).Next(); err == nil {
		t.Error("wrong magic: want error")
	}
}

// TestReadRealArchive reads a real gzip'd initramfs when
// STAGE0_TEST_INITRAMFS names one (none is checked in: they are large).
func TestReadRealArchive(t *testing.T) {
	path := os.Getenv("STAGE0_TEST_INITRAMFS")
	if path == "" {
		t.Skip("set STAGE0_TEST_INITRAMFS to a gzip'd initramfs")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(z)
	n := 0
	for {
		_, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("entry %d: %v", n, err)
		}
		n++
	}
	t.Logf("%d entries", n)
}
