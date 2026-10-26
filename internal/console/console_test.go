package console

import (
	"bytes"
	"context"
	"io"
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
	src := &chunks{[]string{"Rocky Linux 10\r\nk53 log", "in: ", "never read"}}
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
