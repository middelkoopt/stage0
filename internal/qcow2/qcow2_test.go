package qcow2

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fixture loads a gzip'd file from testdata (tools/make-fixtures.sh).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
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

func TestConvert(t *testing.T) {
	want := fixture(t, "disk.raw.gz")
	for _, name := range []string{"plain.qcow2.gz", "zlib.qcow2.gz"} {
		t.Run(name, func(t *testing.T) {
			img, err := Open(bytes.NewReader(fixture(t, name)))
			if err != nil {
				t.Fatal(err)
			}
			if img.Size() != int64(len(want)) {
				t.Fatalf("size %d, want %d", img.Size(), len(want))
			}
			var out bytes.Buffer
			n, err := img.WriteTo(&out)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(want)) || !bytes.Equal(out.Bytes(), want) {
				t.Errorf("converted image differs from the raw one (%d bytes)", n)
			}
			// Random access across a cluster boundary and in the hole.
			buf := make([]byte, 100)
			if _, err := img.ReadAt(buf, 65536-50); err != nil || !bytes.Equal(buf, want[65536-50:65536+50]) {
				t.Errorf("ReadAt across a cluster: %v", err)
			}
			if _, err := img.ReadAt(buf, 2<<20); err != nil || !bytes.Equal(buf, want[2<<20:2<<20+100]) {
				t.Errorf("ReadAt in the hole: %v", err)
			}
		})
	}
}

func TestRejects(t *testing.T) {
	if _, err := Open(bytes.NewReader([]byte("not a qcow2 image at all, just text....................................................................................................."))); err == nil {
		t.Error("garbage accepted")
	}
	b := fixture(t, "plain.qcow2.gz")
	bk := append([]byte(nil), b...)
	bk[15] = 1 // backing file offset (bytes 8-15)
	if _, err := Open(bytes.NewReader(bk)); err == nil {
		t.Error("image with a backing file accepted")
	}
	enc := append([]byte(nil), b...)
	enc[35] = 1 // crypt method (bytes 32-35)
	if _, err := Open(bytes.NewReader(enc)); err == nil {
		t.Error("encrypted image accepted")
	}
}

// TestRealImage converts a real cloud image when STAGE0_TEST_QCOW2 names
// one, checking only that it reads through.
func TestRealImage(t *testing.T) {
	path := os.Getenv("STAGE0_TEST_QCOW2")
	if path == "" {
		t.Skip("set STAGE0_TEST_QCOW2 to a qcow2 image")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := img.WriteTo(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d bytes, sha256 %x", n, h.Sum(nil))
}

// zeroWriterAt is a disk of zeros that records what is written.
type zeroWriterAt struct {
	b      []byte
	writes int
}

func (z *zeroWriterAt) WriteAt(p []byte, off int64) (int, error) {
	z.writes++
	return copy(z.b[off:], p), nil
}

func TestWriteSparse(t *testing.T) {
	want := fixture(t, "disk.raw.gz")
	img, err := Open(bytes.NewReader(fixture(t, "zlib.qcow2.gz")))
	if err != nil {
		t.Fatal(err)
	}
	disk := &zeroWriterAt{b: make([]byte, len(want))}
	n, err := img.WriteSparse(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.b, want) {
		t.Error("sparse write differs from the raw image")
	}
	// 4 MiB in 64 KiB clusters is 64; the pattern fills about 6 and the
	// tail one more: the hole is skipped.
	if disk.writes > 10 || n > 10*65536 {
		t.Errorf("%d writes, %d bytes: the hole was written", disk.writes, n)
	}
}
