// Package cpio reads and writes "newc" cpio archives, the format the Linux
// kernel unpacks as an initramfs. Archives are built in Go, so making a boot
// image needs no root (device nodes are just entries) and no host tools.
package cpio

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
)

const (
	magic      = "070701"
	headerLen  = 110
	trailer    = "TRAILER!!!"
	modeDir    = 0o040000
	modeFile   = 0o100000
	modeChar   = 0o020000
	modeSymlnk = 0o120000
	modeType   = 0o170000
)

// Header describes one entry.
type Header struct {
	Name                 string // relative, no leading "/"
	Mode                 fs.FileMode
	Size                 int64
	RdevMajor, RdevMinor uint32
}

// Writer writes an archive. Entries keep their order; the kernel creates
// them in that order, so parent directories come first.
type Writer struct {
	w   io.Writer
	n   int64 // bytes written, for padding
	ino uint32
}

// NewWriter returns a writer to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Dir adds a directory.
func (w *Writer) Dir(name string, perm fs.FileMode) error {
	return w.header(name, modeDir|uint32(perm.Perm()), 0, 0, 0)
}

// CharDev adds a character device node.
func (w *Writer) CharDev(name string, perm fs.FileMode, major, minor uint32) error {
	return w.header(name, modeChar|uint32(perm.Perm()), 0, major, minor)
}

// File adds a regular file of size bytes read from r.
func (w *Writer) File(name string, perm fs.FileMode, size int64, r io.Reader) error {
	if err := w.header(name, modeFile|uint32(perm.Perm()), size, 0, 0); err != nil {
		return err
	}
	n, err := io.CopyN(w.w, r, size)
	w.n += n
	if err != nil {
		return fmt.Errorf("cpio: %s: wrote %d of %d bytes: %w", name, n, size, err)
	}
	return w.pad()
}

// Close writes the trailer. It does not close the underlying writer.
func (w *Writer) Close() error {
	return w.header(trailer, 0, 0, 0, 0)
}

func (w *Writer) header(name string, mode uint32, size int64, major, minor uint32) error {
	if size < 0 || size > 0xffffffff {
		return fmt.Errorf("cpio: %s: size %d does not fit the format", name, size)
	}
	w.ino++
	nlink := uint32(1)
	if mode&modeType == modeDir {
		nlink = 2
	}
	ino := w.ino
	if name == trailer {
		ino, nlink = 0, 1
	}
	fields := []uint32{ino, mode, 0, 0, nlink, 0, uint32(size), 0, 0, major, minor, uint32(len(name) + 1), 0}
	b := make([]byte, 0, headerLen+len(name)+1)
	b = append(b, magic...)
	for _, f := range fields {
		b = fmt.Appendf(b, "%08x", f)
	}
	b = append(append(b, name...), 0)
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	w.n += int64(len(b))
	return w.pad()
}

func (w *Writer) pad() error {
	if r := w.n % 4; r != 0 {
		n, err := w.w.Write(make([]byte, 4-r))
		w.n += int64(n)
		return err
	}
	return nil
}

// Reader reads an archive entry by entry.
type Reader struct {
	r    *bufio.Reader
	n    int64 // bytes consumed, for padding
	left int64 // unread data in the current entry
	pad  int64 // padding after the current entry's data
}

// NewReader returns a reader of r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReader(r)} }

// Next skips to the next entry and returns its header; io.EOF after the
// trailer.
func (r *Reader) Next() (Header, error) {
	if err := r.skip(r.left + r.pad); err != nil {
		return Header{}, err
	}
	r.left, r.pad = 0, 0
	b := make([]byte, headerLen)
	if err := r.full(b); err != nil {
		return Header{}, err
	}
	if string(b[:6]) != magic {
		return Header{}, fmt.Errorf("cpio: not a newc archive (magic %q)", b[:6])
	}
	var f [13]uint32
	for i := range f {
		v, err := strconv.ParseUint(string(b[6+8*i:14+8*i]), 16, 32)
		if err != nil {
			return Header{}, fmt.Errorf("cpio: bad header field %d: %w", i, err)
		}
		f[i] = uint32(v)
	}
	if f[11] == 0 {
		return Header{}, errors.New("cpio: empty name")
	}
	name := make([]byte, f[11])
	if err := r.full(name); err != nil {
		return Header{}, err
	}
	if err := r.skip(r.align()); err != nil {
		return Header{}, err
	}
	h := Header{Name: string(name[:len(name)-1]), Size: int64(f[6]), RdevMajor: f[9], RdevMinor: f[10]}
	if h.Name == trailer {
		return Header{}, io.EOF
	}
	h.Mode = fs.FileMode(f[1] & 0o777)
	switch f[1] & modeType {
	case modeDir:
		h.Mode |= fs.ModeDir
	case modeChar:
		h.Mode |= fs.ModeDevice | fs.ModeCharDevice
	case modeSymlnk:
		h.Mode |= fs.ModeSymlink
	case modeFile:
	default:
		h.Mode |= fs.ModeIrregular
	}
	r.left = h.Size
	r.pad = (4 - (r.n+h.Size)%4) % 4
	return h, nil
}

// Read reads the current entry's data.
func (r *Reader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	r.left -= int64(n)
	if err == io.EOF && r.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *Reader) full(b []byte) error {
	n, err := io.ReadFull(r.r, b)
	r.n += int64(n)
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (r *Reader) skip(n int64) error {
	m, err := r.r.Discard(int(n))
	r.n += int64(m)
	return err
}

func (r *Reader) align() int64 { return (4 - r.n%4) % 4 }
