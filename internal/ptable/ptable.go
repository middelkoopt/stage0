// Package ptable reads a disk image's partition table, GPT or MBR, so stage0
// can find a partition inside an image without host tools. Sectors are
// taken to be 512 bytes, as in the disk images distros ship.
package ptable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

const sector = 512

// Partition is one partition. Start and Size are in bytes.
type Partition struct {
	Number int // 1-based, as the kernel names them (mmcblk0p1)
	Start  int64
	Size   int64
	Type   string // GPT: type GUID; MBR: two hex digits
	UUID   string // GPT only: the partition's own GUID (PARTUUID)
	Name   string // GPT only
}

// Read reads the partition table at the start of r.
func Read(r io.ReaderAt) ([]Partition, error) {
	mbr := make([]byte, sector)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return nil, fmt.Errorf("ptable: %w", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return nil, errors.New("ptable: no partition table")
	}
	if mbr[446+4] == 0xee {
		return readGPT(r)
	}
	var parts []Partition
	for i := range 4 {
		e := mbr[446+16*i : 446+16*(i+1)]
		start, size := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
		if e[4] == 0 || size == 0 {
			continue
		}
		parts = append(parts, Partition{
			Number: i + 1, Start: int64(start) * sector, Size: int64(size) * sector,
			Type: fmt.Sprintf("%02x", e[4]),
		})
	}
	if len(parts) == 0 {
		return nil, errors.New("ptable: empty MBR")
	}
	return parts, nil
}

func readGPT(r io.ReaderAt) ([]Partition, error) {
	h := make([]byte, sector)
	if _, err := r.ReadAt(h, sector); err != nil {
		return nil, fmt.Errorf("ptable: GPT header: %w", err)
	}
	if string(h[:8]) != "EFI PART" {
		return nil, errors.New("ptable: protective MBR but no GPT header")
	}
	lba, n, size := binary.LittleEndian.Uint64(h[72:]), binary.LittleEndian.Uint32(h[80:]), binary.LittleEndian.Uint32(h[84:])
	if size < 128 || n > 1024 {
		return nil, fmt.Errorf("ptable: GPT with %d entries of %d bytes", n, size)
	}
	ents := make([]byte, int(n)*int(size))
	if _, err := r.ReadAt(ents, int64(lba)*sector); err != nil {
		return nil, fmt.Errorf("ptable: GPT entries: %w", err)
	}
	var parts []Partition
	for i := range int(n) {
		e := ents[i*int(size) : (i+1)*int(size)]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		first, last := binary.LittleEndian.Uint64(e[32:]), binary.LittleEndian.Uint64(e[40:])
		name := make([]uint16, 36)
		for j := range name {
			name[j] = binary.LittleEndian.Uint16(e[56+2*j:])
		}
		for j, c := range name {
			if c == 0 {
				name = name[:j]
				break
			}
		}
		parts = append(parts, Partition{
			Number: i + 1, Start: int64(first) * sector, Size: int64(last-first+1) * sector,
			Type: guid(e[0:16]), UUID: guid(e[16:32]), Name: string(utf16.Decode(name)),
		})
	}
	return parts, nil
}

// guid formats a GUID stored mixed-endian, as GPT stores them.
func guid(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint16(b[4:]),
		binary.LittleEndian.Uint16(b[6:]), b[8:10], b[10:16])
}
