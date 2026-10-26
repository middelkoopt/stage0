package ptable

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fixture loads a gzip'd disk image from testdata
// (tools/make-fixtures.sh makes them).
func fixture(t *testing.T, name string) *bytes.Reader {
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
	return bytes.NewReader(b)
}

func TestGPT(t *testing.T) {
	parts, err := Read(fixture(t, "gpt.img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Partition{
		{Number: 1, Start: 2048 * 512, Size: 4096 * 512, Name: "boot", Type: "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"},
		{Number: 2, Start: 6144 * 512, Size: 8192 * 512, Name: "root", Type: "0fc63daf-8483-4772-8e79-3d69d8477de4",
			UUID: "ae437f98-7402-4a21-9752-30303d4a7a8a"},
	}
	if len(parts) != len(want) {
		t.Fatalf("partitions %+v", parts)
	}
	for i := range want {
		got := parts[i]
		if want[i].UUID == "" {
			got.UUID = "" // random
		}
		if got != want[i] {
			t.Errorf("partition %d = %+v\nwant %+v", i+1, got, want[i])
		}
	}
}

func TestMBR(t *testing.T) {
	parts, err := Read(fixture(t, "dos.img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Partition{
		{Number: 1, Start: 2048 * 512, Size: 4096 * 512, Type: "0c"},
		{Number: 2, Start: 6144 * 512, Size: 8192 * 512, Type: "83"},
	}
	if len(parts) != 2 || parts[0] != want[0] || parts[1] != want[1] {
		t.Errorf("partitions %+v\nwant %+v", parts, want)
	}
}

func TestNoTable(t *testing.T) {
	if _, err := Read(bytes.NewReader(make([]byte, 64<<10))); err == nil {
		t.Error("blank disk: want error")
	}
}

// TestReadRealImage reads a real raw disk image when STAGE0_TEST_IMAGE
// names one (none is checked in: they are large).
func TestReadRealImage(t *testing.T) {
	path := os.Getenv("STAGE0_TEST_IMAGE")
	if path == "" {
		t.Skip("set STAGE0_TEST_IMAGE to a raw disk image")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parts, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		t.Logf("%+v", p)
	}
}
