// Package httpd serves boot files over HTTP, read-only, from one directory:
// the same files as TFTP, for what is too big or too slow for TFTP (a Pi's
// bootloader gives each TFTP file a fixed time). Every request is logged.
package httpd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"
)

// Port is the HTTP port stage0 serves on.
const Port = 80

// Server serves files under a root directory.
type Server struct {
	files fs.FS // confined to the root, symlinks included
	log   *slog.Logger
}

// New returns a server for the files under dir.
func New(dir string, log *slog.Logger) (*Server, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("http root: %w", err)
	}
	return &Server{files: root.FS(), log: log}, nil
}

// Listen opens the HTTP port on addr, pinned to one interface.
func Listen(ctx context.Context, ifname string, addr netip.Addr) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
		})
		return errors.Join(err, serr)
	}}
	ln, err := lc.Listen(ctx, "tcp4", netip.AddrPortFrom(addr, Port).String())
	if err != nil {
		return nil, fmt.Errorf("http on %s: %w", ifname, err)
	}
	return ln, nil
}

// Serve answers GET and HEAD for files on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() {
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	})
	defer stop()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ServeHTTP serves one file; directories are not listed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		d := time.Since(start)
		attrs := []any{"path", r.URL.Path, "from", remoteIP(r.RemoteAddr), "status", lw.status, "bytes", lw.n, "duration", d.Round(time.Millisecond)}
		switch {
		case lw.status == http.StatusOK && r.Method == http.MethodGet:
			s.log.Info("http: sent", append(attrs, "MBps", rate(lw.n, d))...)
		case lw.status >= 400:
			s.log.Info("http: not served", attrs...)
		default:
			s.log.Info("http: answered", attrs...)
		}
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(lw, "read-only", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if !fs.ValidPath(name) || name == "." {
		http.NotFound(lw, r)
		return
	}
	info, err := fs.Stat(s.files, name)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(lw, r)
		return
	}
	http.ServeFileFS(lw, r, s.files, name)
}

// logWriter records the status and body size of a response.
type logWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *logWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func remoteIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// rate is n bytes over d in MB/s, to one decimal.
func rate(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(int(float64(n)/d.Seconds()/1e5)) / 10
}
