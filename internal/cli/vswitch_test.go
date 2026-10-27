package cli

import (
	"bytes"
	"encoding/binary"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/middelkoopt/stage0/internal/vswitch"
)

// pipePort is a fake tap: frames the switch writes arrive on out.
type pipePort struct {
	in  chan []byte
	out chan []byte
}

func (p *pipePort) ReadFrame() ([]byte, error) {
	f, ok := <-p.in
	if !ok {
		return nil, net.ErrClosed
	}
	return f, nil
}
func (p *pipePort) WriteFrame(f []byte) error { p.out <- f; return nil }
func (p *pipePort) Close() error {
	defer func() { recover() }()
	close(p.in)
	return nil
}

func TestVSwitch(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "vnet.sock")
	tap := &pipePort{in: make(chan []byte), out: make(chan []byte, 4)}
	app := &App{OpenTap: func(name string) (vswitch.Port, error) {
		if name != "stage0tap0" {
			t.Errorf("tap %q", name)
		}
		return tap, nil
	}}
	done := make(chan error)
	go func() {
		_, err := runApp(t, app, "vswitch", "--socket", sock, "--timeout", "2s")
		done <- err
	}()
	var c net.Conn
	for i := 0; i < 50 && c == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		c, _ = net.Dial("unix", sock)
	}
	if c == nil {
		t.Fatal("no switch socket")
	}
	defer c.Close()
	// A broadcast from the VM reaches the tap.
	f := append(bytes.Repeat([]byte{0xff}, 6), 0x52, 0x54, 0, 0x12, 0x34, 0x56, 0x08, 0x06, 'h', 'i')
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(f)))
	c.Write(append(hdr[:], f...))
	select {
	case got := <-tap.out:
		if !bytes.Equal(got, f) {
			t.Errorf("tap got % x", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("nothing reached the tap")
	}
	if err := <-done; err != nil {
		t.Errorf("vswitch: %v", err)
	}
}
