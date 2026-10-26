// Package fat reads FAT12, FAT16 and FAT32 filesystems, read-only, as an
// io/fs.FS: enough to take the boot files out of a disk image's boot
// partition without mounting it or using host tools. Long file names are
// supported; names match case-insensitively, as FAT does.
package fat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf16"
)

// FS is an open FAT filesystem.
type FS struct {
	r           io.ReaderAt
	bits        int // 12, 16 or 32
	clusterSize int64
	dataStart   int64  // byte offset of cluster 2
	rootStart   int64  // FAT12/16: byte offset of the fixed root directory
	rootSize    int64  // FAT12/16: its size
	rootCluster uint32 // FAT32: first cluster of the root directory
	clusters    uint32 // number of data clusters
	table       []byte // the first FAT
}

// Open reads the FAT filesystem in r, which is size bytes.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	b := make([]byte, 512)
	if _, err := r.ReadAt(b, 0); err != nil {
		return nil, fmt.Errorf("fat: boot sector: %w", err)
	}
	le16 := func(o int) int64 { return int64(binary.LittleEndian.Uint16(b[o:])) }
	le32 := func(o int) int64 { return int64(binary.LittleEndian.Uint32(b[o:])) }
	if b[510] != 0x55 || b[511] != 0xaa {
		return nil, errors.New("fat: no boot sector signature")
	}
	bps, spc, reserved, fats, rootEnts := le16(11), int64(b[13]), le16(14), int64(b[16]), le16(17)
	if bps < 512 || bps&(bps-1) != 0 || spc == 0 || spc&(spc-1) != 0 || fats == 0 || reserved == 0 {
		return nil, errors.New("fat: not a FAT boot sector")
	}
	total := le16(19)
	if total == 0 {
		total = le32(32)
	}
	fatSize := le16(22)
	if fatSize == 0 {
		fatSize = le32(36)
	}
	rootSectors := (rootEnts*32 + bps - 1) / bps
	firstData := reserved + fats*fatSize + rootSectors
	if total <= firstData || total*bps > size {
		return nil, fmt.Errorf("fat: %d sectors do not fit in %d bytes", total, size)
	}
	f := &FS{
		r:           r,
		clusterSize: spc * bps,
		dataStart:   firstData * bps,
		rootStart:   (reserved + fats*fatSize) * bps,
		rootSize:    rootSectors * bps,
		clusters:    uint32((total - firstData) / spc),
	}
	switch {
	case f.clusters < 4085:
		f.bits = 12
	case f.clusters < 65525:
		f.bits = 16
	default:
		f.bits = 32
		f.rootCluster = uint32(le32(44))
	}
	f.table = make([]byte, fatSize*bps)
	if _, err := r.ReadAt(f.table, reserved*bps); err != nil {
		return nil, fmt.Errorf("fat: reading the FAT: %w", err)
	}
	return f, nil
}

// next returns the cluster after c, or ok false at the end of the chain.
func (f *FS) next(c uint32) (n uint32, ok bool, err error) {
	switch f.bits {
	case 12:
		o := int(c) + int(c)/2
		if o+1 >= len(f.table) {
			return 0, false, errors.New("fat: cluster past the FAT")
		}
		v := uint32(binary.LittleEndian.Uint16(f.table[o:]))
		if c&1 == 1 {
			v >>= 4
		}
		n = v & 0xfff
		ok = n < 0xff8
	case 16:
		if 2*int(c)+1 >= len(f.table) {
			return 0, false, errors.New("fat: cluster past the FAT")
		}
		n = uint32(binary.LittleEndian.Uint16(f.table[2*c:]))
		ok = n < 0xfff8
	default:
		if 4*int(c)+3 >= len(f.table) {
			return 0, false, errors.New("fat: cluster past the FAT")
		}
		n = binary.LittleEndian.Uint32(f.table[4*c:]) & 0x0fffffff
		ok = n < 0x0ffffff8
	}
	if ok && (n < 2 || n >= f.clusters+2) {
		return 0, false, fmt.Errorf("fat: bad cluster %d after %d", n, c)
	}
	return n, ok, nil
}

// read reads size bytes (all of the chain when size < 0) starting at
// cluster start.
func (f *FS) read(start uint32, size int64) ([]byte, error) {
	var out []byte
	if start == 0 { // an empty file
		return out, nil
	}
	c := start
	for steps := uint32(0); ; steps++ {
		if steps > f.clusters || c < 2 || c >= f.clusters+2 {
			return nil, fmt.Errorf("fat: bad cluster chain from %d", start)
		}
		buf := make([]byte, f.clusterSize)
		if _, err := f.r.ReadAt(buf, f.dataStart+int64(c-2)*f.clusterSize); err != nil {
			return nil, err
		}
		out = append(out, buf...)
		if size >= 0 && int64(len(out)) >= size {
			return out[:size], nil
		}
		n, ok, err := f.next(c)
		if err != nil {
			return nil, err
		}
		if !ok {
			if size >= 0 {
				return nil, fmt.Errorf("fat: chain from %d ends before %d bytes", start, size)
			}
			return out, nil
		}
		c = n
	}
}

// entry is a directory entry.
type entry struct {
	name    string
	dir     bool
	cluster uint32
	size    int64
	mod     time.Time
}

// readDir parses the directory at cluster c (0: the root).
func (f *FS) readDir(c uint32) ([]entry, error) {
	var raw []byte
	var err error
	switch {
	case c == 0 && f.bits != 32:
		raw = make([]byte, f.rootSize)
		_, err = f.r.ReadAt(raw, f.rootStart)
	case c == 0:
		raw, err = f.read(f.rootCluster, -1)
	default:
		raw, err = f.read(c, -1)
	}
	if err != nil {
		return nil, err
	}
	var out []entry
	var long []uint16
	for o := 0; o+32 <= len(raw); o += 32 {
		e := raw[o : o+32]
		switch {
		case e[0] == 0:
			return out, nil
		case e[0] == 0xe5:
			long = nil
			continue
		case e[11]&0x3f == 0x0f: // a long-name part; they come last part first
			var part []uint16
			for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
				for i := r[0]; i < r[1]; i += 2 {
					part = append(part, binary.LittleEndian.Uint16(e[i:]))
				}
			}
			if e[0]&0x40 != 0 {
				long = nil
			}
			long = append(part, long...)
			continue
		case e[11]&0x08 != 0: // volume label
			long = nil
			continue
		}
		name := shortName(e)
		if long != nil {
			if i := nameEnd(long); i >= 0 {
				long = long[:i]
			}
			name = string(utf16.Decode(long))
			long = nil
		}
		if name == "." || name == ".." {
			continue
		}
		out = append(out, entry{
			name:    name,
			dir:     e[11]&0x10 != 0,
			cluster: uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:])),
			size:    int64(binary.LittleEndian.Uint32(e[28:])),
			mod:     dosTime(binary.LittleEndian.Uint16(e[24:]), binary.LittleEndian.Uint16(e[22:])),
		})
	}
	return out, nil
}

// nameEnd is where a long name ends: its NUL, or the 0xffff padding.
func nameEnd(s []uint16) int {
	for i, x := range s {
		if x == 0 || x == 0xffff {
			return i
		}
	}
	return -1
}

// shortName is an 8.3 name, lowercased where the entry's flags say so.
func shortName(e []byte) string {
	base := strings.TrimRight(string(e[0:8]), " ")
	ext := strings.TrimRight(string(e[8:11]), " ")
	if base != "" && base[0] == 0x05 {
		base = "\xe5" + base[1:]
	}
	if e[12]&0x08 != 0 {
		base = strings.ToLower(base)
	}
	if e[12]&0x10 != 0 {
		ext = strings.ToLower(ext)
	}
	if ext == "" {
		return base
	}
	return base + "." + ext
}

func dosTime(d, t uint16) time.Time {
	if d == 0 {
		return time.Time{}
	}
	return time.Date(1980+int(d>>9), time.Month(d>>5&0xf), int(d&0x1f),
		int(t>>11), int(t>>5&0x3f), int(t&0x1f)*2, 0, time.UTC)
}

// Open opens a file or directory, matching names case-insensitively.
func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	cur := entry{name: ".", dir: true}
	if name != "." {
		for _, part := range strings.Split(name, "/") {
			if !cur.dir {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
			ents, err := f.readDir(cur.cluster)
			if err != nil {
				return nil, &fs.PathError{Op: "open", Path: name, Err: err}
			}
			found := false
			for _, e := range ents {
				if strings.EqualFold(e.name, part) {
					cur, found = e, true
					break
				}
			}
			if !found {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
		}
	}
	if cur.dir {
		ents, err := f.readDir(cur.cluster)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &dirFile{info: info{cur}, ents: ents}, nil
	}
	data, err := f.read(cur.cluster, cur.size)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &file{Reader: bytes.NewReader(data), info: info{cur}}, nil
}

// info is a FileInfo and DirEntry for an entry.
type info struct{ e entry }

func (i info) Name() string { return path.Base(i.e.name) }
func (i info) Size() int64 {
	if i.e.dir {
		return 0
	}
	return i.e.size
}
func (i info) Mode() fs.FileMode {
	if i.e.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i info) ModTime() time.Time         { return i.e.mod }
func (i info) IsDir() bool                { return i.e.dir }
func (i info) Sys() any                   { return nil }
func (i info) Type() fs.FileMode          { return i.Mode().Type() }
func (i info) Info() (fs.FileInfo, error) { return i, nil }
func (i info) String() string             { return fs.FormatFileInfo(i) }

type file struct {
	*bytes.Reader
	info info
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }

type dirFile struct {
	info info
	ents []entry
	pos  int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.e.name, Err: errors.New("is a directory")}
}

func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.ents[d.pos:]
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	if n > 0 && n < len(rest) {
		rest = rest[:n]
	}
	d.pos += len(rest)
	out := make([]fs.DirEntry, len(rest))
	for i, e := range rest {
		out[i] = info{e}
	}
	return out, nil
}
