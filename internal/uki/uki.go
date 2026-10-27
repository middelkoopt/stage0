// Package uki builds a Unified Kernel Image: systemd's EFI stub with the
// kernel, initrd, kernel command line and os-release added as PE sections,
// one EFI application that UEFI firmware netboots without a bootloader
// (https://uapi-group.org/specifications/specs/unified_kernel_image/).
package uki

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Parts are what goes into the image.
type Parts struct {
	Linux   []byte // an uncompressed kernel Image (see Kernel)
	Initrd  []byte
	Cmdline string
	OSRel   string // os-release contents, for boot menus; optional
}

const (
	sectionHeaderSize = 40
	// IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ
	dataSection = 0x40000040
)

// Build returns stub (systemd's linux<arch>.efi.stub) with parts added
// as the sections .osrel, .cmdline, .initrd and .linux, after the stub's
// own, each at the stub's section and file alignment.
func Build(stub []byte, parts Parts) ([]byte, error) {
	le := binary.LittleEndian
	if len(stub) < 0x40 || string(stub[:2]) != "MZ" {
		return nil, errors.New("stub: not a PE file")
	}
	peOff := int(le.Uint32(stub[0x3c:]))
	if peOff+24 > len(stub) || string(stub[peOff:peOff+4]) != "PE\x00\x00" {
		return nil, errors.New("stub: no PE header")
	}
	coff := peOff + 4
	nsec := int(le.Uint16(stub[coff+2:]))
	opt := coff + 20
	optSize := int(le.Uint16(stub[coff+16:]))
	if le.Uint16(stub[opt:]) != 0x20b {
		return nil, errors.New("stub: not PE32+")
	}
	secAlign := le.Uint32(stub[opt+32:])
	fileAlign := le.Uint32(stub[opt+36:])
	headersSize := int(le.Uint32(stub[opt+60:]))
	table := opt + optSize

	type section struct {
		name string
		data []byte
	}
	var add []section
	if parts.OSRel != "" {
		add = append(add, section{".osrel", []byte(parts.OSRel)})
	}
	if parts.Cmdline != "" {
		add = append(add, section{".cmdline", []byte(parts.Cmdline)})
	}
	if len(parts.Initrd) > 0 {
		add = append(add, section{".initrd", parts.Initrd})
	}
	add = append(add, section{".linux", parts.Linux})
	if table+(nsec+len(add))*sectionHeaderSize > headersSize {
		return nil, fmt.Errorf("stub: no room in its headers for %d more sections", len(add))
	}

	// Where the stub's sections end, in memory and in the file.
	var vend, fend uint32
	for i := range nsec {
		h := stub[table+i*sectionHeaderSize:]
		vend = max(vend, le.Uint32(h[12:])+le.Uint32(h[8:]))
		fend = max(fend, le.Uint32(h[20:])+le.Uint32(h[16:]))
	}
	out := bytes.Clone(stub[:max(int(fend), headersSize)])
	initData := le.Uint32(out[opt+8:])
	for i, s := range add {
		va := alignUp(vend, secAlign)
		raw := alignUp(uint32(len(out)), fileAlign)
		rawSize := alignUp(uint32(len(s.data)), fileAlign)
		out = append(out, make([]byte, int(raw)-len(out))...)
		out = append(out, s.data...)
		out = append(out, make([]byte, int(rawSize)-len(s.data))...)
		h := out[table+(nsec+i)*sectionHeaderSize:][:sectionHeaderSize]
		clear(h)
		copy(h, s.name)
		le.PutUint32(h[8:], uint32(len(s.data))) // virtual size
		le.PutUint32(h[12:], va)
		le.PutUint32(h[16:], rawSize)
		le.PutUint32(h[20:], raw)
		le.PutUint32(h[36:], dataSection)
		vend = va + uint32(len(s.data))
		initData += rawSize
	}
	le.PutUint16(out[coff+2:], uint16(nsec+len(add)))
	le.PutUint32(out[opt+8:], initData)
	le.PutUint32(out[opt+56:], alignUp(vend, secAlign)) // SizeOfImage
	le.PutUint32(out[opt+64:], 0)                       // CheckSum: firmware doesn't check it
	return out, nil
}

func alignUp(n, a uint32) uint32 { return (n + a - 1) / a * a }

// Kernel returns the kernel Image in b: b itself, or, for an arm64 EFI
// zboot kernel (an EFI wrapper "zimg" around a compressed Image, as
// Rocky's vmlinuz is), its payload decompressed.
func Kernel(b []byte) ([]byte, error) {
	if len(b) < 0x20 || string(b[:2]) != "MZ" || string(b[4:8]) != "zimg" {
		return b, nil
	}
	le := binary.LittleEndian
	off, size := le.Uint32(b[8:]), le.Uint32(b[12:])
	if uint64(off)+uint64(size) > uint64(len(b)) {
		return nil, errors.New("zboot kernel: payload past the end")
	}
	payload := bytes.NewReader(b[off : off+size])
	comp := string(bytes.TrimRight(b[0x18:0x20], "\x00"))
	var r io.Reader
	switch comp {
	case "gzip":
		z, err := gzip.NewReader(payload)
		if err != nil {
			return nil, err
		}
		r = z
	case "zstd", "zstd22":
		z, err := zstd.NewReader(payload)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		r = z
	default:
		return nil, fmt.Errorf("zboot kernel: %q compression isn't supported", comp)
	}
	return io.ReadAll(r)
}
