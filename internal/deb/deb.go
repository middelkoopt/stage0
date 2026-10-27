// Package deb reads files out of a Debian package (.deb): an ar archive
// whose data.tar.{xz,zst,gz} holds the files. stage0 uses it for parts no
// image carries, such as UEFI firmware and systemd's UKI stub.
package deb

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// File returns the contents of name (an absolute path the package
// installs) from the .deb read from r.
func File(r io.Reader, name string) ([]byte, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != "!<arch>\n" {
		return nil, errors.New("not a .deb (no ar header)")
	}
	want := path.Clean(strings.TrimPrefix(name, "/"))
	for {
		h := make([]byte, 60)
		if _, err := io.ReadFull(br, h); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("not a .deb (no data.tar)")
			}
			return nil, err
		}
		member := strings.TrimSuffix(strings.TrimSpace(string(h[:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("ar member %q: bad size", member)
		}
		body := io.LimitReader(br, size)
		if strings.HasPrefix(member, "data.tar") {
			return fromTar(body, member, want, name)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return nil, err
		}
		if size%2 == 1 { // members are 2-byte aligned
			br.ReadByte()
		}
	}
}

// fromTar finds want in the compressed tar member.
func fromTar(r io.Reader, member, want, name string) ([]byte, error) {
	var tr io.Reader
	switch path.Ext(member) {
	case ".xz":
		z, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		tr = z
	case ".zst":
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		tr = z
	case ".gz":
		z, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		tr = z
	default:
		return nil, fmt.Errorf("%s: unsupported compression", member)
	}
	t := tar.NewReader(tr)
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: not in the package", name)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(strings.TrimPrefix(h.Name, "./")) == want && h.Typeflag == tar.TypeReg {
			return io.ReadAll(t)
		}
	}
}
