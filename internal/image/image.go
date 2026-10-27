// Package image handles the disk images stage0 installs: one config file
// per image (images/<name>.yaml in the repo), downloading and verifying the
// image into a cache, and taking the boot files out of its boot partition,
// all in Go.
package image

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ulikunitz/xz"
	"go.yaml.in/yaml/v3"

	"github.com/middelkoopt/stage0/internal/fat"
	"github.com/middelkoopt/stage0/internal/ptable"
)

// Config is one image's config file.
type Config struct {
	Name string `yaml:"-"` // the file's name without .yaml

	URL    string `yaml:"url"`    // an xz-compressed raw disk image
	SHA256 string `yaml:"sha256"` // of the file at URL

	// BootPartition holds the firmware's boot files (Pi: config.txt,
	// kernels, device trees); default 1.
	BootPartition int `yaml:"bootPartition,omitempty"`

	// Cmdline is the kernel command line the installed system needs,
	// written as the boot partition's cmdline.txt.
	Cmdline string `yaml:"cmdline"`

	// Ready matches the installed system's console once it has booted;
	// default "login: ".
	Ready string `yaml:"ready,omitempty"`

	// Config are config.txt lines (Pi firmware settings) to ensure, for the
	// agent's boot and the installed system's (see EnsureConfig).
	Config []string `yaml:"config,omitempty"`

	// BootFiles are files (name relative to the boot partition, contents)
	// the seed writes there, e.g. Raspberry Pi OS's empty "ssh" that turns
	// its SSH server on.
	BootFiles map[string]string `yaml:"bootFiles,omitempty"`

	// Kernel and Initrd boot the agent on a VM (a distro's stock kernel
	// and initrd; the agent is appended to the initrd). Images with them
	// are VM images: installed by QEMU booting these directly.
	Kernel *File `yaml:"kernel,omitempty"`
	Initrd *File `yaml:"initrd,omitempty"`

	// BootDir is the directory of the boot partition the firmware reads
	// the kernel, initramfs and cmdline.txt from (Ubuntu's os_prefix,
	// "current"); default the partition's top.
	BootDir string `yaml:"bootDir,omitempty"`

	// Initramfs is the image's own initramfs (path in the boot partition):
	// stage0's agent is appended to it, so the agent has the image's
	// kernel modules. Without it the agent boots alone (Pi kernels with
	// their drivers built in).
	Initramfs string `yaml:"initramfs,omitempty"`

	// NoCloud: the image's cloud-init takes its hostname and keys from
	// NoCloud data that stage0 serves.
	NoCloud bool `yaml:"nocloud,omitempty"`
}

// File is a file to download, pinned by its checksum.
type File struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
}

// File is the file's name, from its URL.
func (f File) File() string { return path.Base(f.URL) }

// VM reports whether the image is installed on VMs (it has an agent kernel).
func (c Config) VM() bool { return c.Kernel != nil }

var (
	validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	hexSum    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Load reads dir/name.yaml.
func Load(dir, name string) (Config, error) {
	var c Config
	if !validName.MatchString(name) {
		return c, fmt.Errorf("image name %q: use lowercase letters, digits, '.', '_' and '-'", name)
	}
	p := filepath.Join(dir, name+".yaml")
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("no image %q (%s)", name, p)
	}
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", p, err)
	}
	c.Name = name
	if c.BootPartition == 0 {
		c.BootPartition = 1
	}
	if c.Ready == "" {
		c.Ready = "login: "
	}
	switch {
	case !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://"):
		return c, fmt.Errorf("%s: url %q is not http(s)", p, c.URL)
	case !strings.HasSuffix(c.URL, ".xz") && !strings.HasSuffix(c.URL, ".qcow2"):
		return c, fmt.Errorf("%s: url %q: only xz-compressed raw disk images (.raw.xz, .img.xz) and qcow2 are supported", p, c.URL)
	case !hexSum.MatchString(c.SHA256):
		return c, fmt.Errorf("%s: sha256 %q is not 64 hex digits", p, c.SHA256)
	case (c.Kernel == nil) != (c.Initrd == nil):
		return c, fmt.Errorf("%s: kernel and initrd go together", p)
	case c.Cmdline == "" && c.Kernel == nil:
		return c, fmt.Errorf("%s: no cmdline", p)
	case c.BootPartition < 0:
		return c, fmt.Errorf("%s: bootPartition %d", p, c.BootPartition)
	}
	for _, f := range []*File{c.Kernel, c.Initrd} {
		if f != nil && (!strings.HasPrefix(f.URL, "http") || !hexSum.MatchString(f.SHA256)) {
			return c, fmt.Errorf("%s: kernel/initrd need an http(s) url and a 64-hex-digit sha256", p)
		}
	}
	for _, p2 := range []string{c.BootDir, c.Initramfs} {
		if p2 != "" && !filepath.IsLocal(p2) {
			return c, fmt.Errorf("%s: %q is not a path inside the boot partition", p, p2)
		}
	}
	for name := range c.BootFiles {
		if !filepath.IsLocal(name) || strings.ContainsAny(name, "\\") {
			return c, fmt.Errorf("%s: bootFiles: %q is not a path inside the boot partition", p, name)
		}
	}
	for _, l := range c.Config {
		if strings.ContainsAny(l, "\r\n") || strings.TrimSpace(l) == "" {
			return c, fmt.Errorf("%s: config: %q is not one line", p, l)
		}
	}
	if _, err := regexp.Compile(c.Ready); err != nil {
		return c, fmt.Errorf("%s: ready: %w", p, err)
	}
	return c, nil
}

// EnsureConfig returns a Pi config.txt with each of lines in force for every
// board: lines already there (exactly, ignoring surrounding space, before
// any section or under [all]) stay as they are; the others are appended in
// an [all] section marked as stage0's. In config.txt the last value of a
// setting wins.
func EnsureConfig(cfg []byte, lines []string) []byte {
	have := map[string]bool{}
	all := true // before any section, or in [all]: applies to every board
	for l := range strings.Lines(string(cfg)) {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			all = l == "[all]"
			continue
		}
		if all {
			have[l] = true
		}
	}
	var add []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if !have[l] {
			add = append(add, l)
			have[l] = true
		}
	}
	if len(add) == 0 {
		return cfg
	}
	out := append([]byte(nil), cfg...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, "\n# Added by stage0.\n[all]\n"...)
	for _, l := range add {
		out = append(out, l+"\n"...)
	}
	return out
}

// SetCloudInitHostname returns cloud-init user-data with its top-level
// hostname set and disable_root false (a precaution, unverified: with its
// default true, cloud-init can rewrite root's authorized_keys to refuse
// logins; Ubuntu's root login worked with it false), editing only those lines (or
// appending them), so the rest of the file, comments included, stays as
// shipped.
func SetCloudInitHostname(userData []byte, hostname string) []byte {
	set := []struct{ key, line string }{
		{"hostname:", "hostname: " + hostname + "\n"},
		{"disable_root:", "disable_root: false\n"},
	}
	found := make([]bool, len(set))
	var out []byte
	for l := range strings.Lines(string(userData)) {
		for i, kv := range set {
			if strings.HasPrefix(l, kv.key) {
				l, found[i] = kv.line, true
			}
		}
		out = append(out, l...)
	}
	for i, kv := range set {
		if !found[i] {
			if len(out) > 0 && out[len(out)-1] != '\n' {
				out = append(out, '\n')
			}
			out = append(out, kv.line...)
		}
	}
	return out
}

// File is the image's file name, from its URL.
func (c Config) File() string { return path.Base(c.URL) }

// Fetch makes sure the image is in cacheDir with the right checksum,
// downloading it if not, and returns its path.
func Fetch(ctx context.Context, c Config, cacheDir string, progress io.Writer) (string, error) {
	return FetchFile(ctx, File{URL: c.URL, SHA256: c.SHA256}, cacheDir, progress)
}

// FetchFile makes sure f is in cacheDir with the right checksum,
// downloading it if not, and returns its path. A download that doesn't
// match the checksum is deleted.
func FetchFile(ctx context.Context, f File, cacheDir string, progress io.Writer) (string, error) {
	dst := filepath.Join(cacheDir, f.File())
	if sum, err := fileSHA256(dst); err == nil && sum == f.SHA256 {
		return dst, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(progress, "downloading %s\n", f.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", f.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(cacheDir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if err = errors.Join(err, tmp.Close()); err != nil {
		return "", fmt.Errorf("download %s: %w", f.URL, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.SHA256 {
		return "", fmt.Errorf("download %s: checksum %s, but the image config says %s", f.URL, sum, f.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	return dst, os.Rename(tmp.Name(), dst)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BootFiles copies every file in partition n of the xz image at imagePath
// (a FAT filesystem) into dst. It decompresses only as far as the end of
// that partition, into a temporary file next to dst.
func BootFiles(imagePath string, n int, dst string) error {
	f, err := os.Open(imagePath)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := xz.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", imagePath, err)
	}

	// The partition table lies in the first MiB of any image stage0 knows.
	head := make([]byte, 1<<20)
	if _, err := io.ReadFull(z, head); err != nil {
		return fmt.Errorf("%s: %w", imagePath, err)
	}
	parts, err := ptable.Read(bytes.NewReader(head))
	if err != nil {
		return fmt.Errorf("%s: %w", imagePath, err)
	}
	var part *ptable.Partition
	for i := range parts {
		if parts[i].Number == n {
			part = &parts[i]
		}
	}
	if part == nil {
		return fmt.Errorf("%s: no partition %d", imagePath, n)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partition-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// Copy the head and the rest of the stream up to the partition's end.
	end := part.Start + part.Size
	if _, err := tmp.Write(head[:min(int64(len(head)), end)]); err != nil {
		return err
	}
	if end > int64(len(head)) {
		if _, err := io.CopyN(tmp, z, end-int64(len(head))); err != nil {
			return fmt.Errorf("%s: partition %d: %w", imagePath, n, err)
		}
	}

	fsys, err := fat.Open(io.NewSectionReader(tmp, part.Start, part.Size), part.Size)
	if err != nil {
		return fmt.Errorf("%s: partition %d: %w", imagePath, n, err)
	}
	return copyFS(dst, fsys)
}

// copyFS writes every directory and file of fsys under dir.
func copyFS(dir string, fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
