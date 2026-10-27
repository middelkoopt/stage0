package uki

import (
	"bytes"
	"compress/gzip"
	"debug/pe"
	"encoding/binary"
	"testing"
)

// stub writes a minimal PE32+ EFI application with one .text section and
// header room for more, like systemd's stub.
func stub(t *testing.T) []byte {
	t.Helper()
	const (
		lfanew      = 0x40
		optSize     = 240
		headersSize = 0x1000
		align       = 0x1000
		fileAlign   = 0x200
	)
	b := make([]byte, headersSize+fileAlign)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], lfanew)
	copy(b[lfanew:], "PE\x00\x00")
	coff := lfanew + 4
	binary.LittleEndian.PutUint16(b[coff:], 0xaa64) // machine
	binary.LittleEndian.PutUint16(b[coff+2:], 1)    // sections
	binary.LittleEndian.PutUint16(b[coff+16:], optSize)
	binary.LittleEndian.PutUint16(b[coff+18:], 0x0206) // executable, large address aware
	opt := coff + 20
	binary.LittleEndian.PutUint16(b[opt:], 0x20b)                // PE32+
	binary.LittleEndian.PutUint32(b[opt+16:], headersSize)       // entry point
	binary.LittleEndian.PutUint64(b[opt+24:], 0x10000000)        // image base
	binary.LittleEndian.PutUint32(b[opt+32:], align)             // section alignment
	binary.LittleEndian.PutUint32(b[opt+36:], fileAlign)         // file alignment
	binary.LittleEndian.PutUint32(b[opt+56:], headersSize+align) // size of image
	binary.LittleEndian.PutUint32(b[opt+60:], headersSize)       // size of headers
	binary.LittleEndian.PutUint16(b[opt+68:], 10)                // EFI application
	binary.LittleEndian.PutUint32(b[opt+108:], 16)               // data directories
	sec := opt + optSize
	copy(b[sec:], ".text")
	binary.LittleEndian.PutUint32(b[sec+8:], 4)            // virtual size
	binary.LittleEndian.PutUint32(b[sec+12:], headersSize) // virtual address
	binary.LittleEndian.PutUint32(b[sec+16:], fileAlign)   // raw size
	binary.LittleEndian.PutUint32(b[sec+20:], headersSize) // raw offset
	binary.LittleEndian.PutUint32(b[sec+36:], 0x60000020)  // code, execute, read
	copy(b[headersSize:], "CODE")
	return b
}

func TestBuild(t *testing.T) {
	out, err := Build(stub(t), Parts{Linux: []byte("KERNEL"), Initrd: []byte("INITRD!"), Cmdline: "console=ttyAMA0", OSRel: "NAME=stage0\n"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := pe.NewFile(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{".text": "CODE", ".osrel": "NAME=stage0\n", ".cmdline": "console=ttyAMA0", ".initrd": "INITRD!", ".linux": "KERNEL"}
	if len(f.Sections) != len(want) {
		t.Fatalf("%d sections", len(f.Sections))
	}
	var end uint32
	for _, s := range f.Sections {
		d, _ := s.Data()
		if w, ok := want[s.Name]; !ok || string(d[:s.VirtualSize]) != w {
			t.Errorf("section %s = %q", s.Name, d[:min(len(d), 16)])
		}
		if s.VirtualAddress < end || s.VirtualAddress%0x1000 != 0 || s.Offset%0x200 != 0 {
			t.Errorf("section %s at %#x (file %#x) overlaps or is unaligned", s.Name, s.VirtualAddress, s.Offset)
		}
		end = s.VirtualAddress + s.VirtualSize
	}
	oh := f.OptionalHeader.(*pe.OptionalHeader64)
	if oh.SizeOfImage < end || oh.SizeOfImage%0x1000 != 0 {
		t.Errorf("SizeOfImage %#x, sections end at %#x", oh.SizeOfImage, end)
	}
}

func TestBuildNoRoom(t *testing.T) {
	s := stub(t)
	// Shrink the header room so new section entries can't fit.
	binary.LittleEndian.PutUint32(s[0x40+4+20+60:], 0x40+4+20+240+40)
	if _, err := Build(s, Parts{Linux: []byte("K")}); err == nil {
		t.Error("no header room: want error")
	}
}

func TestKernelZboot(t *testing.T) {
	image := []byte("MZ arm64 Image")
	var gz bytes.Buffer
	z := gzip.NewWriter(&gz)
	z.Write(image)
	z.Close()
	// zboot: "MZ", "zimg", payload offset and size, then the compression.
	hdr := make([]byte, 64)
	copy(hdr, "MZ\x00\x00zimg")
	binary.LittleEndian.PutUint32(hdr[8:], 64)
	binary.LittleEndian.PutUint32(hdr[12:], uint32(gz.Len()))
	copy(hdr[0x18:], "gzip")
	got, err := Kernel(append(hdr, gz.Bytes()...))
	if err != nil || !bytes.Equal(got, image) {
		t.Errorf("Kernel(zboot) = %q, %v", got, err)
	}
	if got, err := Kernel(image); err != nil || !bytes.Equal(got, image) {
		t.Errorf("Kernel(Image) = %q, %v", got, err)
	}
	copy(hdr[0x18:], "lz4\x00")
	if _, err := Kernel(append(hdr, gz.Bytes()...)); err == nil {
		t.Error("lz4 zboot: want error")
	}
}
