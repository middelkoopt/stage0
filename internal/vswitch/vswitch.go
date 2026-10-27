// Package vswitch is a virtual Ethernet switch for the QEMU test bed: VMs
// join it over a Unix socket (QEMU's -netdev stream), and it forwards
// frames between them like a learning switch: unicast to the port a MAC was
// last seen on, everything else to every other port. No root is needed,
// and nothing reaches a real network unless a port bridges to one.
package vswitch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// maxFrame bounds a frame (jumbo frames included).
const maxFrame = 65535

// Port is one connection to the switch.
type Port interface {
	ReadFrame() ([]byte, error)
	WriteFrame([]byte) error
	Close() error
}

// Switch forwards frames between its ports.
type Switch struct {
	log *slog.Logger

	mu    sync.Mutex
	ports map[Port]bool
	macs  map[[6]byte]Port
}

// New returns an empty switch.
func New(log *slog.Logger) *Switch {
	return &Switch{log: log, ports: map[Port]bool{}, macs: map[[6]byte]Port{}}
}

// Serve attaches every connection made to ln until ctx is done.
func (s *Switch) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.closeAll()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Attach(Stream(c))
		}()
	}
}

// Attach forwards p's frames until it ends, then detaches it.
func (s *Switch) Attach(p Port) {
	s.mu.Lock()
	s.ports[p] = true
	n := len(s.ports)
	s.mu.Unlock()
	s.log.Info("vswitch: port attached", "ports", n)
	defer s.detach(p)
	for {
		f, err := p.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Warn("vswitch: port read", "err", err)
			}
			return
		}
		if len(f) < 14 {
			continue
		}
		s.forward(p, f)
	}
}

func (s *Switch) forward(from Port, f []byte) {
	var dst, src [6]byte
	copy(dst[:], f[0:6])
	copy(src[:], f[6:12])
	s.mu.Lock()
	if src[0]&1 == 0 { // learn unicast sources only
		s.macs[src] = from
	}
	var to []Port
	if p, ok := s.macs[dst]; ok && dst[0]&1 == 0 {
		if p != from {
			to = []Port{p}
		}
	} else {
		for p := range s.ports {
			if p != from {
				to = append(to, p)
			}
		}
	}
	s.mu.Unlock()
	for _, p := range to {
		if err := p.WriteFrame(f); err != nil {
			s.log.Debug("vswitch: port write", "err", err)
		}
	}
}

func (s *Switch) detach(p Port) {
	s.mu.Lock()
	delete(s.ports, p)
	for mac, q := range s.macs {
		if q == p {
			delete(s.macs, mac)
		}
	}
	n := len(s.ports)
	s.mu.Unlock()
	p.Close()
	s.log.Info("vswitch: port detached", "ports", n)
}

func (s *Switch) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.ports {
		p.Close()
	}
}

// Stream is a port over a stream connection in QEMU's -netdev stream
// framing: each frame is preceded by its length, 4 bytes big-endian.
func Stream(c net.Conn) Port { return &stream{c: c} }

type stream struct {
	c  net.Conn
	mu sync.Mutex // writes come from several ports' goroutines
}

func (s *stream) ReadFrame() ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(s.c, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > maxFrame {
		return nil, fmt.Errorf("vswitch: frame of %d bytes", n)
	}
	f := make([]byte, n)
	_, err := io.ReadFull(s.c, f)
	return f, err
}

func (s *stream) WriteFrame(f []byte) error {
	b := make([]byte, 4+len(f))
	binary.BigEndian.PutUint32(b, uint32(len(f)))
	copy(b[4:], f)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.c.Write(b)
	return err
}

func (s *stream) Close() error { return s.c.Close() }

// Tap is a port on a tap device (a kernel network interface): frames
// written here appear on the interface, and frames the kernel sends out of
// it come back here. The device must exist and be owned by the user, e.g.
// one a systemd-networkd .netdev with [Tap] User= creates.
func Tap(name string) (Port, error) {
	// Attach the descriptor before Go's poller sees it: /dev/net/tun only
	// becomes pollable once it is a tap.
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("vswitch: tap %s: %w", name, err)
	}
	const (
		tunsetiff = 0x400454ca
		iffTap    = 0x0002
		iffNoPI   = 0x1000 // frames without the 4-byte packet-info header
	)
	var req [40]byte // struct ifreq: name, then flags
	copy(req[:15], name)
	binary.NativeEndian.PutUint16(req[16:], iffTap|iffNoPI)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tunsetiff, uintptr(unsafe.Pointer(&req[0]))); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vswitch: tap %s (does it exist, and is it yours?): %w", name, errno)
	}
	return &tap{f: os.NewFile(uintptr(fd), "/dev/net/tun:"+name)}, nil
}

type tap struct{ f *os.File }

func (t *tap) ReadFrame() ([]byte, error) {
	buf := make([]byte, maxFrame)
	n, err := t.f.Read(buf)
	return buf[:n], err
}

func (t *tap) WriteFrame(f []byte) error {
	_, err := t.f.Write(f)
	return err
}

func (t *tap) Close() error { return t.f.Close() }
