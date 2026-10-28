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
	"path/filepath"
	"strings"

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

// Append writes base to w, then what then writes, as one initramfs: the
// kernel unpacks archives one after another. Between them it skips zeros
// but takes an uncompressed cpio only at a 4-byte boundary (k52: a
// 37,939,845-byte dracut image followed directly by one was "not
// initramfs"), so base is padded to one.
func Append(w io.Writer, base io.Reader, then func(io.Writer) error) error {
	n, err := io.Copy(w, base)
	if err != nil {
		return err
	}
	if _, err := w.Write(make([]byte, (4-n%4)%4)); err != nil {
		return err
	}
	return then(w)
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

// hookScript is the dracut hook that runs the agent from an installer's
// initramfs. dracut sources pre-mount hooks once the network is up
// (rd.neednet=1); the agent installs and reboots, so it never returns.
const hookScript = `#!/bin/sh
# stage0: install from the container (the agent reboots when done).
/usr/bin/stage0 agent </dev/console >/dev/console 2>&1
`

// hookOrder is a systemd drop-in that runs dracut's pre-mount hooks (the
// agent) after the installer's Ignition has partitioned the disk, when it
// does (on k52 they ran at once otherwise); without Ignition it does
// nothing.
const hookOrder = `[Unit]
After=ignition-disks.service
`

// BuildHook writes, as a gzip'd cpio for appending to a dracut initramfs
// (an installer's, docs/containers.md), the stage0 binary and the dracut
// hook that runs it as the agent.
func BuildHook(w io.Writer, agentPath string) error {
	z, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	a := cpio.NewWriter(z)
	for _, d := range []string{"usr", "usr/bin", "var", "var/lib", "var/lib/dracut", "var/lib/dracut/hooks", "var/lib/dracut/hooks/pre-mount",
		"etc", "etc/systemd", "etc/systemd/system", "etc/systemd/system/dracut-pre-mount.service.d"} {
		if err := a.Dir(d, 0o755); err != nil {
			return err
		}
	}
	if _, err := addFile(a, "usr/bin/stage0", 0o755, agentPath); err != nil {
		return err
	}
	if err := a.File("var/lib/dracut/hooks/pre-mount/90-stage0.sh", 0o755, int64(len(hookScript)), strings.NewReader(hookScript)); err != nil {
		return err
	}
	if err := a.File("etc/systemd/system/dracut-pre-mount.service.d/90-stage0.conf", 0o644, int64(len(hookOrder)), strings.NewReader(hookOrder)); err != nil {
		return err
	}
	return errors.Join(a.Close(), z.Close())
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

// BuildSeed writes the files under dir as a seed: a plain (uncompressed)
// cpio of directories and regular files, in sorted order, that the agent
// unpacks over the installed image's boot partition. It returns the number
// of files.
func BuildSeed(w io.Writer, dir string) (int, error) {
	a := cpio.NewWriter(w)
	n := 0
	err := fs.WalkDir(os.DirFS(dir), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		switch {
		case d.IsDir():
			return a.Dir(name, 0o755)
		case d.Type().IsRegular():
			n++
			_, err := addFile(a, name, 0o644, filepath.Join(dir, name))
			return err
		}
		return fmt.Errorf("seed: %s: only files and directories are supported", name)
	})
	if err != nil {
		return n, err
	}
	return n, a.Close()
}
