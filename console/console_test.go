package console

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// chunks returns its chunks one Read at a time, then EOF, like a serial port
// that delivers output in pieces.
type chunks struct{ c []string }

func (r *chunks) Read(p []byte) (int, error) {
	if len(r.c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.c[0])
	r.c = r.c[1:]
	return n, nil
}

// idle never has data: each Read returns nothing, as a serial port does when
// its read timeout passes.
type idle struct{}

func (idle) Read([]byte) (int, error) {
	time.Sleep(time.Millisecond)
	return 0, nil
}

func TestCaptureCopiesUntilEOF(t *testing.T) {
	var out bytes.Buffer
	m, err := Capture(context.Background(), &chunks{[]string{"U-Boot\r\n", "Linux\r\n"}}, &out, nil)
	if err != nil || m != "" {
		t.Fatalf("match %q, err %v", m, err)
	}
	if out.String() != "U-Boot\r\nLinux\r\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestCaptureStopsAtMatch(t *testing.T) {
	var out bytes.Buffer
	// The empty chunk is the port going quiet after the prompt.
	src := &chunks{[]string{"Rocky Linux 10\r\nk53 log", "in: ", "", "never read"}}
	m, err := Capture(context.Background(), src, &out, regexp.MustCompile(`login: `))
	if err != nil {
		t.Fatal(err)
	}
	if m != "login: " {
		t.Errorf("match = %q", m)
	}
	if out.String() != "Rocky Linux 10\r\nk53 login: " {
		t.Errorf("out = %q; should stop after the chunk that matched", out.String())
	}
}

func TestCaptureNoMatchBeforeEOF(t *testing.T) {
	_, err := Capture(context.Background(), &chunks{[]string{"panic\r\n"}}, io.Discard, regexp.MustCompile(`login:`))
	if err == nil {
		t.Error("output ended without a match: want error")
	}
}

func TestCaptureStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	m, err := Capture(ctx, idle{}, io.Discard, nil)
	if err != nil || m != "" {
		t.Errorf("match %q, err %v; without --until, the end of the context is a normal stop", m, err)
	}
}

func TestCaptureTimesOutWaitingForMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Capture(ctx, idle{}, io.Discard, regexp.MustCompile(`login:`)); err == nil {
		t.Error("want an error when the pattern never appears")
	}
}

func TestCaptureKeepsOnlyRecentOutputForMatching(t *testing.T) {
	big := string(bytes.Repeat([]byte("x"), 3*tail))
	m, err := Capture(context.Background(), &chunks{[]string{"lo", big, "gin: "}}, io.Discard, regexp.MustCompile(`login: `))
	if err == nil || m != "" {
		t.Errorf("match %q, err %v; text split by more than the window should not match", m, err)
	}
}

func TestCaptureWaitsForTheRestOfAMatch(t *testing.T) {
	// A long line arrives in pieces; a match that reaches the end of what
	// has arrived so far may grow, so it is taken only once more output
	// comes, the port goes quiet (an empty read), or the output ends.
	re := regexp.MustCompile(`STAGE0-REPORT .*`)
	src := &chunks{[]string{"boot\r\nSTAGE0-REPORT {", `"disks":[]}`, "\r\nmore", "never read"}}
	m, err := Capture(context.Background(), src, io.Discard, re)
	if err != nil || m != `STAGE0-REPORT {"disks":[]}`+"\r" {
		t.Errorf("match = %q, %v", m, err)
	}

	// A prompt with no newline after it is taken when the port goes quiet.
	src = &chunks{[]string{"k52 login: ", "", "never read"}}
	if m, err := Capture(context.Background(), src, io.Discard, regexp.MustCompile(`login: `)); err != nil || m != "login: " {
		t.Errorf("prompt match = %q, %v", m, err)
	}
}

func TestOpenUnixSocket(t *testing.T) {
	// A VM's serial console, as QEMU serves it with -chardev socket.
	sock := filepath.Join(t.TempDir(), "console.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("vm1 login: "))
		buf := make([]byte, 16)
		n, _ := c.Read(buf)
		c.Write(buf[:n])
		c.Close()
	}()
	p, err := Open("unix:"+sock, 115200)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	io.WriteString(p, "root\r")
	b, _ := io.ReadAll(p)
	if string(b) != "vm1 login: root\r" {
		t.Errorf("read %q", b)
	}
}
