package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckFresh(t *testing.T) {
	dir := t.TempDir()
	copyPath := filepath.Join(t.TempDir(), "stage0")
	os.WriteFile(copyPath, []byte("OLD"), 0o755)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(copyPath, old, old)
	if err := checkFresh(copyPath, dir); err != nil {
		t.Errorf("no ./stage0: %v", err)
	}
	local := filepath.Join(dir, "stage0")
	os.WriteFile(local, []byte("NEW"), 0o755)
	if err := checkFresh(copyPath, dir); err == nil || !strings.Contains(err.Error(), "install-capped.sh") {
		t.Errorf("stale copy: err = %v", err)
	}
	os.WriteFile(local, []byte("OLD"), 0o755) // rebuilt, same bytes
	if err := checkFresh(copyPath, dir); err != nil {
		t.Errorf("same contents: %v", err)
	}
	os.WriteFile(local, []byte("NEWER"), 0o755)
	os.Chtimes(local, old.Add(-time.Hour), old.Add(-time.Hour)) // ./stage0 older
	if err := checkFresh(copyPath, dir); err != nil {
		t.Errorf("older ./stage0: %v", err)
	}
	if err := checkFresh(local, dir); err != nil {
		t.Errorf("running ./stage0 itself: %v", err)
	}
}
