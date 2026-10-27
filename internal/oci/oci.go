// Package oci reads a container image from an OCI image layout directory
// (what `podman save --format oci-dir` writes): its labels, and files
// from its layers, so stage0 can boot a container's kernel and installer
// without a container runtime.
package oci

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type descriptor struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	Manifests     []descriptor `json:"manifests"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type config struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Labels map[string]string `json:"Labels,omitempty"`
	} `json:"config"`
}

// Image is one image in a layout.
type Image struct {
	Architecture string
	Labels       map[string]string
	Digest       string // of its manifest, "sha256:<hex>"
	open         opener
	layers       []descriptor
}

// opener opens a file of a layout by its path in it ("index.json",
// "blobs/sha256/<hex>").
type opener func(name string) (io.ReadCloser, error)

// Open reads the image in the layout at dir, which must hold exactly one.
func Open(dir string) (*Image, error) {
	open := func(name string) (io.ReadCloser, error) { return os.Open(filepath.Join(dir, filepath.FromSlash(name))) }
	var idx index
	if err := readJSON(open, "index.json", "", &idx); err != nil {
		return nil, fmt.Errorf("%s: not an OCI image layout: %w", dir, err)
	}
	if len(idx.Manifests) != 1 {
		return nil, fmt.Errorf("%s: %d images in the layout, want 1", dir, len(idx.Manifests))
	}
	img, err := load(open, idx.Manifests[0].Digest)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return img, nil
}

// OpenURL reads the image with manifest digest from the layout served at
// base (http://…/oci). Every blob is checked against its digest.
func OpenURL(base, digest string) (*Image, error) {
	open := func(name string) (io.ReadCloser, error) {
		resp, err := http.Get(strings.TrimSuffix(base, "/") + "/" + name)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s/%s: %s", base, name, resp.Status)
		}
		return resp.Body, nil
	}
	img, err := load(open, digest)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", base, err)
	}
	return img, nil
}

// load reads the manifest with digest, and its config.
func load(open opener, digest string) (*Image, error) {
	var m manifest
	if err := readJSON(open, blobPath(digest), digest, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var c config
	if err := readJSON(open, blobPath(m.Config.Digest), m.Config.Digest, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &Image{Architecture: c.Architecture, Labels: c.Config.Labels, Digest: digest, open: open, layers: m.Layers}, nil
}

// Files returns the named files (absolute or not) as the image's merged
// layers have them: a later layer's file replaces an earlier one's, and a
// whiteout (.wh.<name>) removes it.
func (img *Image) Files(names ...string) (map[string][]byte, error) {
	want := map[string]string{} // clean path in the tar -> name asked for
	for _, n := range names {
		want[path.Clean(strings.TrimPrefix(n, "/"))] = n
	}
	found := map[string][]byte{}
	for _, l := range img.layers {
		if err := img.scan(l, func(name string, h *tar.Header, r io.Reader) error {
			dir, base := path.Split(name)
			if w, ok := strings.CutPrefix(base, ".wh."); ok {
				if n, ok := want[path.Join(dir, w)]; ok {
					delete(found, n)
				}
				return nil
			}
			n, ok := want[name]
			if !ok {
				return nil
			}
			if h.Typeflag != tar.TypeReg {
				return fmt.Errorf("%s: not a regular file in the image", n)
			}
			b, err := io.ReadAll(r)
			found[n] = b
			return err
		}); err != nil {
			return nil, err
		}
	}
	for _, n := range names {
		if _, ok := found[n]; !ok {
			return nil, fmt.Errorf("%s: not in the image", n)
		}
	}
	return found, nil
}

// Extract writes the directory dir of the image (its merged layers) to
// dst: regular files and directories; whiteouts remove what an earlier
// layer put there.
func (img *Image) Extract(dir, dst string) error {
	prefix := path.Clean(strings.TrimPrefix(dir, "/"))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dst) // confines every write to dst
	if err != nil {
		return err
	}
	defer root.Close()
	for _, l := range img.layers {
		// Whiteouts hide only lower layers' entries, wherever they come
		// in the layer's tar.
		written := map[string]bool{}
		if err := img.scan(l, func(name string, h *tar.Header, r io.Reader) error {
			rel, ok := strings.CutPrefix(name, prefix+"/")
			if !ok || !filepath.IsLocal(rel) {
				return nil
			}
			if ok, err := whiteout(root, rel, written); ok || err != nil {
				return err
			}
			d, _ := path.Split(rel)
			switch {
			case h.Typeflag == tar.TypeDir:
				written[rel] = true
				return root.MkdirAll(rel, 0o755)
			case h.Typeflag == tar.TypeReg:
				written[rel] = true
				if d != "" {
					if err := root.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				f, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
				if err != nil {
					return err
				}
				_, err = io.Copy(f, r)
				return errors.Join(err, f.Close())
			default:
				return fmt.Errorf("%s/%s: links and special files aren't supported here", dir, rel)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// Unpack writes the whole image (its layers in order, whiteouts applied)
// to dst, an empty directory: files, directories, symlinks and hard links
// with their modes, times and extended attributes (file capabilities),
// and their owners when run as root. Device nodes and FIFOs are skipped
// (devtmpfs makes /dev on a booted system). It returns the bytes of file
// data written.
func (img *Image) Unpack(dst string) (int64, error) {
	root, err := os.OpenRoot(dst) // confines every write to dst
	if err != nil {
		return 0, err
	}
	defer root.Close()
	var n int64
	for _, l := range img.layers {
		written := map[string]bool{}
		if err := img.scan(l, func(name string, h *tar.Header, r io.Reader) error {
			if h.Typeflag == tar.TypeReg {
				n += h.Size
			}
			return unpackEntry(root, written, name, h, r)
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// unpackEntry writes one tar entry (name cleaned, relative) under root;
// written collects what this layer wrote, for whiteouts.
func unpackEntry(root *os.Root, written map[string]bool, name string, h *tar.Header, r io.Reader) error {
	if name == "." || !filepath.IsLocal(name) {
		return nil
	}
	if ok, err := whiteout(root, name, written); ok || err != nil {
		return err
	}
	mode := h.FileInfo().Mode()
	owner := os.Geteuid() == 0
	if d := path.Dir(name); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// A new entry replaces what a lower layer had there, but a directory
	// stays a directory.
	if fi, err := root.Lstat(name); err == nil && !(fi.IsDir() && h.Typeflag == tar.TypeDir) {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if owner {
			if err := root.Lchown(name, h.Uid, h.Gid); err != nil {
				return err
			}
		}
		if err := root.Chmod(name, mode); err != nil {
			return err
		}
	case tar.TypeReg:
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		err = writeFile(f, h, r, owner, mode)
		if err := errors.Join(err, f.Close()); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := root.Chtimes(name, h.ModTime, h.ModTime); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := root.Symlink(h.Linkname, name); err != nil {
			return err
		}
		if owner {
			if err := root.Lchown(name, h.Uid, h.Gid); err != nil {
				return err
			}
		}
	case tar.TypeLink:
		target := path.Clean(strings.TrimPrefix(h.Linkname, "/"))
		if !filepath.IsLocal(target) {
			return fmt.Errorf("%s: hard link to %s, outside the image", name, h.Linkname)
		}
		if err := root.Link(target, name); err != nil {
			return err
		}
	default: // devices, FIFOs
		return nil
	}
	written[name] = true
	return nil
}

// writeFile fills a new regular file from the tar entry, then sets its
// extended attributes, owner and mode (owner first: chown clears setuid).
func writeFile(f *os.File, h *tar.Header, r io.Reader, owner bool, mode fs.FileMode) error {
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	for k, v := range h.PAXRecords {
		if attr, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			if err := unix.Fsetxattr(int(f.Fd()), attr, []byte(v), 0); err != nil {
				return fmt.Errorf("xattr %s: %w", attr, err)
			}
		}
	}
	if owner {
		if err := f.Chown(h.Uid, h.Gid); err != nil {
			return err
		}
	}
	return f.Chmod(mode)
}

// whiteout applies name if it is a whiteout (.wh.<name> removes a lower
// layer's entry; .wh..wh..opq empties a directory of them), reporting
// whether it was one.
func whiteout(root *os.Root, name string, written map[string]bool) (bool, error) {
	d, base := path.Split(name)
	switch {
	case base == ".wh..wh..opq":
		return true, clearDir(root, path.Clean(d), written)
	case strings.HasPrefix(base, ".wh."):
		return true, root.RemoveAll(path.Join(d, strings.TrimPrefix(base, ".wh.")))
	}
	return false, nil
}

// clearDir removes what is in dir under root, except what this layer
// wrote (keep).
func clearDir(root *os.Root, dir string, keep map[string]bool) error {
	f, err := root.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		p := path.Join(dir, n)
		if keep[p] {
			continue
		}
		if err := root.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// scan calls f for each entry of a layer, then checks the layer's digest.
func (img *Image) scan(l descriptor, f func(name string, h *tar.Header, r io.Reader) error) error {
	file, err := img.open(blobPath(l.Digest))
	if err != nil {
		return err
	}
	defer file.Close()
	sum, err := newDigest(l.Digest)
	if err != nil {
		return err
	}
	var r io.Reader = io.TeeReader(file, sum)
	switch l.MediaType {
	case "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip":
		z, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		r = z
	case "application/vnd.oci.image.layer.v1.tar":
	default:
		return fmt.Errorf("layer %s: unsupported media type %q", l.Digest, l.MediaType)
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		if err := f(path.Clean(strings.TrimPrefix(h.Name, "/")), h, tr); err != nil {
			return err
		}
	}
	// Read what the tar reader left (padding, gzip trailer) so the digest
	// covers the whole blob.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("layer %s: %w", l.Digest, err)
	}
	if _, err := io.Copy(sum, file); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(sum.Sum(nil)); got != l.Digest {
		return fmt.Errorf("layer %s: digest is %s", l.Digest, got)
	}
	return nil
}

func newDigest(d string) (hash.Hash, error) {
	if !strings.HasPrefix(d, "sha256:") {
		return nil, fmt.Errorf("digest %q: only sha256 is supported", d)
	}
	return sha256.New(), nil
}

// blobPath is a blob's path in a layout.
func blobPath(digest string) string {
	alg, hex, _ := strings.Cut(digest, ":")
	return "blobs/" + alg + "/" + hex
}

// readJSON decodes the layout's file name into v, first checking it
// against digest unless that is empty.
func readJSON(open opener, name, digest string, v any) error {
	f, err := open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return err
	}
	if digest != "" {
		if _, err := newDigest(digest); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
			return fmt.Errorf("%s: digest is %s, want %s", name, got, digest)
		}
	}
	return json.Unmarshal(b, v)
}
