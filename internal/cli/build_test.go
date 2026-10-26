package cli

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/middelkoopt/stage0/internal/cpio"
)

func TestBuildInitrd(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "stage0")
	os.WriteFile(agent, []byte("AGENT"), 0o755)
	out := filepath.Join(dir, "initramfs8")
	checked := ""
	app := &App{CheckStatic: func(p string) error { checked = p; return nil }}
	got, err := runApp(t, app, "build", "initrd", "--out", out, "--agent", agent, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if checked != agent {
		t.Errorf("checked %q, want the agent", checked)
	}
	var s struct {
		Path       string `json:"path"`
		AgentBytes int64  `json:"agentBytes"`
	}
	if err := json.Unmarshal([]byte(got), &s); err != nil || s.Path != out || s.AgentBytes != 5 {
		t.Errorf("output %s (%v)", got, err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	r := cpio.NewReader(z)
	found := false
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || h.Name == "init"
	}
	if !found {
		t.Error("no /init in the archive")
	}
}

func TestBuildInitrdRefusesDynamic(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "initramfs8")
	app := &App{CheckStatic: func(string) error { return errors.New("x is dynamically linked") }}
	if _, err := runApp(t, app, "build", "initrd", "--out", out); err == nil || !strings.Contains(err.Error(), "dynamically") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("wrote an initramfs anyway")
	}
}
