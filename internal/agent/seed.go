package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/middelkoopt/stage0/internal/cpio"
)

// The seed is a cpio archive of first-boot files (cmdline.txt, cloud-init
// data, ...) that the agent unpacks over the installed image's boot
// partition, its first partition. Files not in the seed stay as shipped.

// ApplySeed unpacks the seed archive r into dir, replacing files that
// exist, and returns the names written. Every name must stay inside dir.
func ApplySeed(r io.Reader, dir string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var names []string
	a := cpio.NewReader(r)
	for {
		h, err := a.Next()
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return names, err
		}
		name := filepath.Clean(h.Name)
		if !filepath.IsLocal(name) {
			return names, fmt.Errorf("seed: %q is outside the boot partition", h.Name)
		}
		switch {
		case h.Mode.IsDir():
			if err := mkdirAll(root, name); err != nil {
				return names, err
			}
		case h.Mode.IsRegular():
			if d := filepath.Dir(name); d != "." {
				if err := mkdirAll(root, d); err != nil {
					return names, err
				}
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return names, err
			}
			_, err = io.Copy(f, a)
			if err = errors.Join(err, f.Close()); err != nil {
				return names, fmt.Errorf("seed: %s: %w", name, err)
			}
			names = append(names, name)
		default:
			return names, fmt.Errorf("seed: %s: only files and directories are supported", name)
		}
	}
}

func mkdirAll(root *os.Root, name string) error {
	parts := strings.Split(name, string(filepath.Separator))
	for i := range parts {
		err := root.Mkdir(filepath.Join(parts[:i+1]...), 0o755)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

// partition is the device name of a disk's nth partition.
func partition(disk string, n int) string {
	if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", disk, n) // mmcblk0p1, nvme0n1p1
	}
	return fmt.Sprintf("%s%d", disk, n) // sda1
}

// seedDisk makes the kernel re-read the disk's new partition table, mounts
// its first partition (FAT) and applies the seed file there.
func seedDisk(disk, seedPath string) ([]string, error) {
	dev, err := os.Open("/dev/" + disk)
	if err != nil {
		return nil, err
	}
	const blkrrpart = 0x125f
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dev.Fd(), blkrrpart, 0)
	dev.Close()
	if errno != 0 {
		return nil, fmt.Errorf("re-reading the partition table of %s: %w", disk, errno)
	}
	part := "/dev/" + partition(disk, 1)
	if err := waitDev(part); err != nil {
		return nil, err
	}
	const mnt = "/mnt/boot"
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return nil, err
	}
	if err := syscall.Mount(part, mnt, "vfat", 0, ""); err != nil {
		return nil, fmt.Errorf("mounting %s: %w", part, err)
	}
	f, err := os.Open(seedPath)
	if err != nil {
		syscall.Unmount(mnt, 0)
		return nil, err
	}
	names, err := ApplySeed(f, mnt)
	f.Close()
	syscall.Sync()
	if uerr := syscall.Unmount(mnt, 0); err == nil && uerr != nil {
		err = fmt.Errorf("unmounting %s: %w", part, uerr)
	}
	return names, err
}
