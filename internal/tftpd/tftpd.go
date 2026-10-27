// Package tftpd serves boot files over TFTP, read-only, from one directory.
// Every request is logged with its outcome: a Pi's bootloader probes for many
// files that don't exist, so the log is how a boot is followed.
package tftpd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pin/tftp/v3"
)

// Server serves files from a root directory.
type Server struct {
	root *os.Root // confines every open to the directory, symlinks included
	log  *slog.Logger
}

// New returns a server for the files under dir.
func New(dir string, log *slog.Logger) (*Server, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("tftp root: %w", err)
	}
	return &Server{root: root, log: log}, nil
}

// Listen opens the TFTP port on addr, pinned to one interface. Binding to
// the address rather than 0.0.0.0 also keeps broadcast requests out, so each
// transfer's own socket (bound by the library to the address the request came
// to) is on addr too.
func Listen(ctx context.Context, ifname string, addr netip.Addr) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
		})
		return errors.Join(err, serr)
	}}
	network := "udp4"
	if addr.Is6() {
		network = "udp6"
	}
	pc, err := lc.ListenPacket(ctx, network, netip.AddrPortFrom(addr, 69).String())
	if err != nil {
		return nil, fmt.Errorf("tftp on %s: %w", ifname, err)
	}
	return pc, nil
}

// Serve answers read requests on conn until ctx is done; writes are refused.
func (s *Server) Serve(ctx context.Context, conn net.PacketConn) error {
	srv := tftp.NewServer(s.read, nil)
	if u, ok := conn.LocalAddr().(*net.UDPAddr); ok && u.IP.To4() == nil {
		// Over IPv6, blocks of 1428 bytes fit a 1500-byte frame; a Pi
		// asks for 1468 (fine over IPv4) and needs this cap (lab notes).
		// One socket for all transfers: per-transfer sockets would bind the
		// request's link-local address without its zone, which fails.
		srv.SetBlockSize(1428)
		srv.EnableSinglePort()
	}
	stop := context.AfterFunc(ctx, srv.Shutdown)
	defer stop()
	err := srv.Serve(conn)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// read sends one file. Names are taken relative to the root whether or not
// they start with "/" (a Pi asks for both).
func (s *Server) read(name string, rf io.ReaderFrom) error {
	log := s.log.With("file", name)
	if ot, ok := rf.(tftp.OutgoingTransfer); ok {
		addr := ot.RemoteAddr()
		log = log.With("from", addr.IP.String())
	}

	rel := filepath.Clean(strings.TrimLeft(name, "/"))
	if !filepath.IsLocal(rel) {
		log.Warn("tftp: refused, outside the root")
		return fmt.Errorf("%s: not found", name)
	}
	f, err := s.root.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		log.Info("tftp: not found")
		return fmt.Errorf("%s: not found", name)
	}
	if err != nil {
		log.Warn("tftp: refused", "err", err)
		return fmt.Errorf("%s: not found", name)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		log.Info("tftp: not found", "reason", "not a regular file")
		return fmt.Errorf("%s: not found", name)
	}

	start := time.Now()
	n, err := rf.ReadFrom(f)
	d := time.Since(start)
	if err != nil && n == 0 && strings.Contains(err.Error(), "Early terminate") {
		// A Pi asks for the size first, aborts, then asks for the data.
		log.Info("tftp: size probe")
		return nil
	}
	if err != nil {
		log.Warn("tftp: transfer failed", "bytes", n, "duration", d.Round(time.Millisecond), "err", err)
		return err
	}
	log.Info("tftp: sent", "bytes", n, "duration", d.Round(time.Millisecond), "MBps", rate(n, d))
	return nil
}

// rate is n bytes over d in MB/s, to one decimal.
func rate(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(int(float64(n)/d.Seconds()/1e5)) / 10
}
