// Package ipmi is a small IPMI 2.0 BMC over the LAN (RMCP+, what
// `ipmitool -I lanplus` speaks), so stage-1 tools can drive a node that
// has no BMC of its own: stage0 answers for it, power backed by its PoE
// port and Serial-over-LAN by its serial console. One Server is one
// node's BMC, on its own UDP port.
//
// Supported: cipher suites 3 (RAKP-HMAC-SHA1, HMAC-SHA1-96, AES-CBC-128)
// and 17 (the same with SHA-256); one user; the commands ipmitool uses for
// chassis power and SOL. Reference: IPMI v2.0 rev 1.1 (sections 13, 15,
// 22, 24, 26, 28).
package ipmi

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Node is what the BMC controls.
type Node interface {
	// PowerOn reports whether the node has power.
	PowerOn(ctx context.Context) (bool, error)
	// SetPower turns the node's power on or off.
	SetPower(ctx context.Context, on bool) error
	// Console opens the node's serial console, for SOL.
	Console() (Console, error)
}

// Console is a serial console: what it prints is read, what is written is
// typed.
type Console interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

// Server is one node's BMC.
type Server struct {
	Name     string // the node, for logs and Get Device ID
	User     string
	Password string // at most 20 bytes
	Node     Node
	Log      *slog.Logger

	guid    [16]byte
	powerMu sync.Mutex // chassis control, one at a time

	mu       sync.Mutex
	sessions map[uint32]*session // by the BMC's session ID
	conn     net.PacketConn
}

// Serve answers on conn until ctx is done.
func (s *Server) Serve(ctx context.Context, conn net.PacketConn) error {
	if len(s.Password) > 20 {
		return errors.New("ipmi: the password is longer than 20 bytes")
	}
	rand.Read(s.guid[:])
	s.mu.Lock()
	s.sessions = map[uint32]*session{}
	s.conn = conn
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	go s.expire(ctx)
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			s.closeAll()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if out := s.handle(ctx, buf[:n], from); out != nil {
			if _, err := conn.WriteTo(out, from); err != nil {
				s.Log.Debug("ipmi: send", "err", err)
			}
		}
	}
}

// sessionIdle is how long a session lives without a message (the spec's
// suggested 60 s).
const sessionIdle = 60 * time.Second

// expire drops idle sessions.
func (s *Server) expire(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			for id, ss := range s.sessions {
				if now.Sub(ss.last) > sessionIdle {
					ss.close()
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ss := range s.sessions {
		ss.close()
		delete(s.sessions, id)
	}
}

// handle answers one datagram, or returns nil.
func (s *Server) handle(ctx context.Context, b []byte, from net.Addr) []byte {
	// RMCP header: version 6, reserved, sequence 0xff (no ack), class 7
	// (IPMI).
	if len(b) < 5 || b[0] != 0x06 || b[3]&0x1f != 0x07 {
		return nil
	}
	b = b[4:]
	if b[0] == authRMCPPlus {
		return s.handle20(ctx, b, from)
	}
	return s.handle15(ctx, b)
}

var rmcpHeader = []byte{0x06, 0x00, 0xff, 0x07}
