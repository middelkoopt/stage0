package tftpd

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pin/tftp/v3"
)

// syncBuffer is a log sink safe for the server's transfer goroutines.
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

// serve starts a server on loopback over a temporary root holding config.txt
// and 563cea77/cmdline.txt, with secret.txt just outside the root.
func serve(t *testing.T) (*tftp.Client, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	files := map[string]string{
		"secret.txt":                "outside the root",
		"root/config.txt":           "[all]\narm_64bit=1\n",
		"root/563cea77/cmdline.txt": "console=serial0,115200",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return serveRoot(t, root, conn)
}

// serveOn serves a root holding config.txt on conn.
func serveOn(t *testing.T, conn net.PacketConn) (*tftp.Client, *syncBuffer) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("[all]\narm_64bit=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return serveRoot(t, root, conn)
}

func serveRoot(t *testing.T, root string, conn net.PacketConn) (*tftp.Client, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	s, err := New(root, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, conn) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	c, err := tftp.NewClient(conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetRetries(1)
	return c, logs
}

func fetch(c *tftp.Client, name string) (string, error) {
	wt, err := c.Receive(name, "octet")
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	_, err = wt.WriteTo(&b)
	return b.String(), err
}

func TestFetch(t *testing.T) {
	c, logs := serve(t)
	for _, name := range []string{"config.txt", "/config.txt", "563cea77/cmdline.txt", "/563cea77/../config.txt"} {
		got, err := fetch(c, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := "arm_64bit"; name != "563cea77/cmdline.txt" && !strings.Contains(got, want) {
			t.Errorf("%s = %q", name, got)
		}
	}
	if l := logs.String(); !strings.Contains(l, "tftp: sent") || !strings.Contains(l, "bytes=18") {
		t.Errorf("log lacks the transfer:\n%s", l)
	}
}

func TestOnlyInsideRoot(t *testing.T) {
	c, logs := serve(t)
	for _, name := range []string{"../secret.txt", "/../secret.txt", "563cea77/../../secret.txt"} {
		if got, err := fetch(c, name); err == nil {
			t.Errorf("%s: served %q from outside the root", name, got)
		}
	}
	if !strings.Contains(logs.String(), "tftp: refused") {
		t.Errorf("log lacks the refusal:\n%s", logs)
	}
}

func TestNotFound(t *testing.T) {
	c, logs := serve(t)
	if _, err := fetch(c, "pieeprom.sig"); err == nil {
		t.Error("missing file: want error")
	}
	if !strings.Contains(logs.String(), "tftp: not found") {
		t.Errorf("log lacks the miss (the Pi probes for files, so misses are routine):\n%s", logs)
	}
}

func TestNoWrites(t *testing.T) {
	c, _ := serve(t)
	rf, err := c.Send("upload.txt", "octet")
	if err == nil {
		_, err = rf.ReadFrom(strings.NewReader("data"))
	}
	if err == nil {
		t.Error("write accepted")
	}
}

func TestSizeProbe(t *testing.T) {
	// A Pi's bootloader and UEFI firmware (edk2) ask for a file's size
	// (tsize), then abort and ask again for the data. That is routine, not
	// a failure. Each aborts its own way: ERROR code and message.
	for _, abort := range []struct {
		code byte
		msg  string
	}{{0, "Early terminate"}, {8, "User aborted the transfer"}} {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, logs := serveOn(t, conn)
		cli, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer cli.Close()
		// RRQ config.txt octet tsize 0
		rrq := append([]byte{0, 1}, "config.txt\x00octet\x00tsize\x000\x00"...)
		if _, err := cli.WriteTo(rrq, conn.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		cli.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 516)
		_, from, err := cli.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		if buf[1] != 6 {
			t.Fatalf("got opcode %d, want an OACK (6)", buf[1])
		}
		// The ERROR, to the transfer's own port.
		if _, err := cli.WriteTo(append([]byte{0, 5, 0, abort.code}, abort.msg+"\x00"...), from); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), "tftp: size probe") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if l := logs.String(); !strings.Contains(l, "tftp: size probe") || strings.Contains(l, "transfer failed") {
			t.Errorf("%q: want a size probe, not a failure:\n%s", abort.msg, l)
		}
	}
}
