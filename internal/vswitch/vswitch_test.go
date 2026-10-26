package vswitch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// frame builds an Ethernet frame from src to dst with a payload.
func frame(dst, src byte, payload string) []byte {
	f := make([]byte, 14, 14+len(payload))
	for i := range 6 {
		f[i], f[6+i] = dst, src
	}
	if dst == 0xff {
		copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	}
	f[12], f[13] = 0x08, 0x00
	return append(f, payload...)
}

func serve(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "switch.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return sock
}

// peer is a test port whose frames arrive on a channel, read by one
// goroutine for its whole life.
type peer struct {
	Port
	frames chan []byte
}

func dial(t *testing.T, sock string) *peer {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	p := &peer{Port: Stream(c), frames: make(chan []byte, 16)}
	go func() {
		for {
			f, err := p.ReadFrame()
			if err != nil {
				return
			}
			p.frames <- f
		}
	}()
	return p
}

// expect waits up to a second for a frame on p.
func expect(t *testing.T, p *peer, want []byte) {
	t.Helper()
	select {
	case f := <-p.frames:
		if !bytes.Equal(f, want) {
			t.Errorf("got frame % x\nwant       % x", f, want)
		}
	case <-time.After(time.Second):
		t.Errorf("no frame, want % x", want)
	}
}

// quiet checks no frame arrives at p for a moment.
func quiet(t *testing.T, p *peer) {
	t.Helper()
	select {
	case f := <-p.frames:
		t.Errorf("unexpected frame % x", f)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestFloodThenLearn(t *testing.T) {
	sock := serve(t)
	a, b, c := dial(t, sock), dial(t, sock), dial(t, sock)
	time.Sleep(50 * time.Millisecond) // let the switch attach them

	// A broadcast from a reaches b and c, not a.
	bc := frame(0xff, 0xa2, "who is b")
	if err := a.WriteFrame(bc); err != nil {
		t.Fatal(err)
	}
	expect(t, b, bc)
	expect(t, c, bc)

	// b answers a, which the switch has learned: only a gets it.
	reply := frame(0xa2, 0xb2, "b here")
	if err := b.WriteFrame(reply); err != nil {
		t.Fatal(err)
	}
	expect(t, a, reply)
	quiet(t, c)

	// To an unknown MAC: flooded.
	unknown := frame(0xcc, 0xa2, "anyone")
	a.WriteFrame(unknown)
	expect(t, b, unknown)
	expect(t, c, unknown)
}

func TestStreamFraming(t *testing.T) {
	// QEMU's stream netdev: each frame is a 4-byte big-endian length, then
	// the frame.
	x, y := net.Pipe()
	p := Stream(x)
	go func() { y.Write([]byte{0, 0, 0, 3, 'a', 'b', 'c'}) }()
	f, err := p.ReadFrame()
	if err != nil || string(f) != "abc" {
		t.Fatalf("read %q, %v", f, err)
	}
	go p.WriteFrame([]byte("xy"))
	buf := make([]byte, 6)
	io.ReadFull(y, buf)
	if !bytes.Equal(buf, []byte{0, 0, 0, 2, 'x', 'y'}) {
		t.Errorf("wrote % x", buf)
	}
}

func TestStreamRejectsHugeFrame(t *testing.T) {
	x, y := net.Pipe()
	go func() { y.Write([]byte{0, 1, 0, 0}) }() // 65536 bytes
	if _, err := Stream(x).ReadFrame(); err == nil {
		t.Error("oversized frame accepted")
	}
}

func TestPortLeaving(t *testing.T) {
	sock := serve(t)
	a, b := dial(t, sock), dial(t, sock)
	time.Sleep(50 * time.Millisecond)
	a.WriteFrame(frame(0xff, 0xa2, "hello"))
	expect(t, b, frame(0xff, 0xa2, "hello"))
	a.Close()
	time.Sleep(50 * time.Millisecond)
	// a's MAC is forgotten: a frame to it is flooded (to b alone now) and
	// nothing breaks.
	c := dial(t, sock)
	time.Sleep(50 * time.Millisecond)
	b.WriteFrame(frame(0xa2, 0xb2, "gone?"))
	expect(t, c, frame(0xa2, 0xb2, "gone?"))
}

// TestQEMU boots a real QEMU VM (UEFI, set to netboot first) on the switch
// when STAGE0_TEST_QEMU names an aarch64 UEFI firmware with a network stack,
// and waits for its first DHCP request: proof the stream framing matches.
// Debian's AAVMF and QEMU_EFI have no network stack (docs/netboot.md), so
// this needs another firmware.
func TestQEMU(t *testing.T) {
	fw := os.Getenv("STAGE0_TEST_QEMU")
	if fw == "" {
		t.Skip("set STAGE0_TEST_QEMU to an aarch64 UEFI firmware, e.g. /usr/share/AAVMF/AAVMF_CODE.fd")
	}
	sock := serve(t)
	p := dial(t, sock)
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// The firmware as flash, with a writable copy of its variable store
	// (AAVMF_VARS.fd next to it), so it creates its default boot entries.
	vars := filepath.Join(t.TempDir(), "vars.fd")
	b, err := os.ReadFile(filepath.Join(filepath.Dir(fw), "AAVMF_VARS.fd"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(vars, b, 0o644)
	cmd := exec.CommandContext(ctx, "qemu-system-aarch64", "-M", "virt", "-accel", "kvm", "-cpu", "host", "-m", "512",
		"-nographic",
		"-drive", "if=pflash,format=raw,readonly=on,file="+fw, "-drive", "if=pflash,format=raw,file="+vars,
		"-netdev", "stream,id=n0,server=off,addr.type=unix,addr.path="+sock,
		"-device", "virtio-net-pci,netdev=n0,mac=52:54:00:12:34:56,bootindex=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	frames := 0
	for {
		select {
		case f := <-p.frames:
			frames++
			// IPv4 (0x0800), UDP (17), from port 68 to 67: DHCP.
			if len(f) > 42 && f[12] == 8 && f[13] == 0 && f[23] == 17 && f[34] == 0 && f[35] == 68 && f[37] == 67 {
				t.Logf("DHCP from %x after the VM started", f[6:12])
				return
			}
		case <-ctx.Done():
			cmd.Process.Kill()
			cmd.Wait()
			tail := out.Bytes()
			if len(tail) > 1500 {
				tail = tail[len(tail)-1500:]
			}
			t.Fatalf("no DHCP request from the VM (%d frames); its console ended with:\n%s", frames, tail)
		}
	}
}
