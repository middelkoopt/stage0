package httpd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func serve(t *testing.T) (string, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside"), 0o644)
	root := filepath.Join(dir, "root")
	os.MkdirAll(filepath.Join(root, "images"), 0o755)
	os.WriteFile(filepath.Join(root, "images", "rocky.raw.xz"), []byte("IMAGE"), 0o644)
	logs := &syncBuffer{}
	s, err := New(root, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return "http://" + ln.Addr().String(), logs
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func TestServeFile(t *testing.T) {
	base, logs := serve(t)
	if code, body := get(t, base+"/images/rocky.raw.xz"); code != 200 || body != "IMAGE" {
		t.Errorf("got %d %q", code, body)
	}
	l := logs.String()
	if !strings.Contains(l, "http: sent") || !strings.Contains(l, "bytes=5") || !strings.Contains(l, "path=/images/rocky.raw.xz") {
		t.Errorf("log lacks the transfer:\n%s", l)
	}
}

func TestServeMissingAndOutside(t *testing.T) {
	base, logs := serve(t)
	for _, p := range []string{"/nope", "/../secret.txt", "/images/../../secret.txt", "/%2e%2e/secret.txt"} {
		if code, body := get(t, base+p); code == 200 {
			t.Errorf("%s: served %q", p, body)
		}
	}
	if !strings.Contains(logs.String(), "status=404") {
		t.Errorf("log lacks the misses:\n%s", logs)
	}
}

func TestNoDirectoryListings(t *testing.T) {
	base, _ := serve(t)
	if code, body := get(t, base+"/images/"); code == 200 {
		t.Errorf("listed a directory: %q", body)
	}
}

func TestReadOnly(t *testing.T) {
	base, _ := serve(t)
	r, err := http.Post(base+"/images/x", "text/plain", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", r.StatusCode)
	}
}
