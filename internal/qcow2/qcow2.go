// Package qcow2 reads QEMU's qcow2 disk images, read-only, as a raw disk:
// enough to install the EL cloud images, which are published for aarch64
// only as qcow2. Plain and zlib-compressed clusters are supported; images
// with a backing file, encryption, external data, extended L2 entries or
// zstd compression are refused.
package qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Image is an open qcow2 image.
type Image struct {
	r           io.ReaderAt
	clusterBits uint
	size        int64
	l1          []uint64
}

// Open reads the image's header and L1 table.
func Open(r io.ReaderAt) (*Image, error) {
	h := make([]byte, 112)
	if _, err := r.ReadAt(h, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("qcow2: header: %w", err)
	}
	be32 := func(o int) uint32 { return binary.BigEndian.Uint32(h[o:]) }
	be64 := func(o int) uint64 { return binary.BigEndian.Uint64(h[o:]) }
	if string(h[:4]) != "QFI\xfb" {
		return nil, errors.New("qcow2: not a qcow2 image")
	}
	version := be32(4)
	img := &Image{clusterBits: uint(be32(20)), size: int64(be64(24))}
	switch {
	case version != 2 && version != 3:
		return nil, fmt.Errorf("qcow2: version %d", version)
	case be64(8) != 0:
		return nil, errors.New("qcow2: images with a backing file are not supported")
	case be32(32) != 0:
		return nil, errors.New("qcow2: encrypted images are not supported")
	case img.clusterBits < 9 || img.clusterBits > 21:
		return nil, fmt.Errorf("qcow2: cluster bits %d", img.clusterBits)
	}
	if version == 3 {
		incompat := be64(72)
		if incompat&^1 != 0 { // bit 0 is "dirty", harmless for reading
			return nil, fmt.Errorf("qcow2: unsupported features %#x (external data, extended L2 or zstd)", incompat)
		}
		if be32(100) > 104 && h[104] != 0 {
			return nil, errors.New("qcow2: only zlib compression is supported")
		}
	}
	n := be32(36)
	if n > 1<<24 {
		return nil, fmt.Errorf("qcow2: L1 table of %d entries", n)
	}
	l1 := make([]byte, 8*int(n))
	if _, err := r.ReadAt(l1, int64(be64(40))); err != nil {
		return nil, fmt.Errorf("qcow2: L1 table: %w", err)
	}
	img.l1 = make([]uint64, n)
	for i := range img.l1 {
		img.l1[i] = binary.BigEndian.Uint64(l1[8*i:])
	}
	img.r = r
	return img, nil
}

// Size is the virtual disk's size in bytes.
func (img *Image) Size() int64 { return img.size }

const offsetMask = 0x00fffffffffffe00 // bits 9-55: a host offset

// cluster returns the virtual cluster that holds offset off.
func (img *Image) cluster(off int64) ([]byte, error) {
	c, _, err := img.clusterData(off)
	return c, err
}

// clusterData returns the cluster that holds off, and whether it holds
// data (false: unallocated or zero, the returned cluster all zeros).
func (img *Image) clusterData(off int64) ([]byte, bool, error) {
	cs := int64(1) << img.clusterBits
	idx := off >> img.clusterBits
	l2n := cs / 8
	l1i, l2i := idx/l2n, idx%l2n
	if l1i >= int64(len(img.l1)) {
		return make([]byte, cs), false, nil
	}
	l2off := int64(img.l1[l1i] & offsetMask)
	if l2off == 0 {
		return make([]byte, cs), false, nil // unallocated: zeros
	}
	var e [8]byte
	if _, err := img.r.ReadAt(e[:], l2off+8*l2i); err != nil {
		return nil, false, fmt.Errorf("qcow2: L2 entry: %w", err)
	}
	entry := binary.BigEndian.Uint64(e[:])
	buf := make([]byte, cs)
	switch {
	case entry&(1<<62) != 0: // compressed
		x := 62 - (img.clusterBits - 8)
		host := int64(entry & (1<<x - 1))
		sectors := int64((entry>>x)&(1<<(img.clusterBits-8)-1)) + 1
		comp := make([]byte, sectors*512-host%512)
		if _, err := img.r.ReadAt(comp, host); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, fmt.Errorf("qcow2: compressed cluster: %w", err)
		}
		if _, err := io.ReadFull(flate.NewReader(bytes.NewReader(comp)), buf); err != nil {
			return nil, false, fmt.Errorf("qcow2: inflating a cluster: %w", err)
		}
	case entry&1 != 0, entry&offsetMask == 0: // reads as zeros
		return buf, false, nil
	default:
		if _, err := img.r.ReadAt(buf, int64(entry&offsetMask)); err != nil {
			return nil, false, fmt.Errorf("qcow2: cluster: %w", err)
		}
	}
	return buf, true, nil
}

// ReadAt reads the virtual disk.
func (img *Image) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		if off >= img.size {
			return n, io.EOF
		}
		c, err := img.cluster(off)
		if err != nil {
			return n, err
		}
		in := off & (int64(len(c)) - 1)
		m := copy(p[n:], c[in:min(int64(len(c)), in+img.size-off)])
		n += m
		off += int64(m)
	}
	return n, nil
}

// WriteTo writes the whole virtual disk to w, as a raw image.
func (img *Image) WriteTo(w io.Writer) (int64, error) {
	cs := int64(1) << img.clusterBits
	var n int64
	for off := int64(0); off < img.size; off += cs {
		c, err := img.cluster(off)
		if err != nil {
			return n, err
		}
		m, err := w.Write(c[:min(cs, img.size-off)])
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// WriteSparse writes the clusters that hold data to w at their offsets,
// skipping unallocated and zero clusters (a fresh or don't-care disk), and
// returns the bytes written.
func (img *Image) WriteSparse(w io.WriterAt) (int64, error) {
	cs := int64(1) << img.clusterBits
	var n int64
	for off := int64(0); off < img.size; off += cs {
		c, data, err := img.clusterData(off)
		if err != nil {
			return n, err
		}
		if !data {
			continue
		}
		m, err := w.WriteAt(c[:min(cs, img.size-off)], off)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
