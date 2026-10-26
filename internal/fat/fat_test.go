package fat

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// fixture loads a gzip'd FAT image from testdata
// (tools/make-fat-fixtures.sh makes them).
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

// kernel is the 5000-byte file the fixtures hold as kernel_2712.img.
func kernel() string {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "kernel%04d\n", i)
	}
	return b.String()
}

func TestRead(t *testing.T) {
	for _, name := range []string{"fat12.img.gz", "fat16.img.gz", "fat32.img.gz"} {
		t.Run(name, func(t *testing.T) {
			r := fixture(t, name)
			fsys, err := Open(r, r.Size())
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"config.txt":      "arm_64bit=1\n",
				"kernel_2712.img": kernel(),
				"overlays/README": "overlay readme\n",
				"overlays/a-rather-long-overlay-name.dtbo": "arm_64bit=1\n",
			}
			for p, body := range want {
				got, err := fs.ReadFile(fsys, p)
				if err != nil {
					t.Errorf("%s: %v", p, err)
					continue
				}
				if string(got) != body {
					t.Errorf("%s: %d bytes, want %d", p, len(got), len(body))
				}
			}
			var files []string
			fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					t.Error(err)
					return err
				}
				if !d.IsDir() {
					files = append(files, p)
				}
				return nil
			})
			slices.Sort(files)
			if got := strings.Join(files, " "); got != "config.txt kernel_2712.img overlays/README overlays/a-rather-long-overlay-name.dtbo" {
				t.Errorf("files: %s", got)
			}
			if _, err := fs.ReadFile(fsys, "missing.txt"); !os.IsNotExist(err) {
				t.Errorf("missing file: err = %v", err)
			}
			if err := fstest.TestFS(fsys, "config.txt", "kernel_2712.img", "overlays/README"); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCaseInsensitive(t *testing.T) {
	r := fixture(t, "fat16.img.gz")
	fsys, err := Open(r, r.Size())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(fsys, "CONFIG.TXT"); err != nil {
		t.Errorf("FAT names are case-insensitive: %v", err)
	}
}

func TestRejectsNonFAT(t *testing.T) {
	r := bytes.NewReader(make([]byte, 4096))
	if _, err := Open(r, r.Size()); err == nil {
		t.Error("zeros accepted as FAT")
	}
}
