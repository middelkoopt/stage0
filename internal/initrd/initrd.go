// Package initrd builds stage0's initramfs: the stage0 binary as /init (it
// runs as the agent when it is PID 1), the directories the kernel and agent
// need, a console node, and an optional payload. It is a gzip'd newc cpio,
// built without root or host tools.
package initrd

import (
	"compress/gzip"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/middelkoopt/stage0/internal/agent"
	"github.com/middelkoopt/stage0/internal/cpio"
)

// Summary describes a built initramfs.
type Summary struct {
	Bytes        int64 `json:"bytes"` // compressed archive
	AgentBytes   int64 `json:"agentBytes"`
	PayloadBytes int64 `json:"payloadBytes,omitempty"`
}

// countWriter counts bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Build writes the initramfs to w. payloadPath may be empty.
func Build(w io.Writer, agentPath, payloadPath string) (Summary, error) {
	var s Summary
	cw := &countWriter{w: w}
	z, err := gzip.NewWriterLevel(cw, gzip.BestSpeed)
	if err != nil {
		return s, err
	}
	a := cpio.NewWriter(z)
	for _, d := range []struct {
		name string
		perm fs.FileMode
	}{{"dev", 0o755}, {"proc", 0o755}, {"sys", 0o755}, {"run", 0o755}, {"tmp", 0o777}} {
		if err := a.Dir(d.name, d.perm); err != nil {
			return s, err
		}
	}
	// The kernel opens /dev/console for init's output before init runs.
	if err := a.CharDev("dev/console", 0o600, 5, 1); err != nil {
		return s, err
	}
	if s.AgentBytes, err = addFile(a, "init", 0o755, agentPath); err != nil {
		return s, err
	}
	if payloadPath != "" {
		if s.PayloadBytes, err = addFile(a, agent.PayloadPath, 0o644, payloadPath); err != nil {
			return s, err
		}
	}
	if err := errors.Join(a.Close(), z.Close()); err != nil {
		return s, err
	}
	s.Bytes = cw.n
	return s, nil
}

func addFile(a *cpio.Writer, name string, perm fs.FileMode, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	return info.Size(), a.File(name, perm, info.Size(), f)
}

// CheckStatic makes sure path is a statically linked ELF executable, which
// is all an empty initramfs can run.
func CheckStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s is not an ELF executable: %w", path, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked; build it with CGO_ENABLED=0", path)
		}
	}
	return nil
}
