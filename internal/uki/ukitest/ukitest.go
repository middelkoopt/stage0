// Package ukitest makes stand-in EFI stubs for tests.
package ukitest

import "encoding/binary"

// Stub returns a minimal PE32+ EFI application with one .text section and
// header room for more, like systemd's stub.
func Stub() []byte {
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
