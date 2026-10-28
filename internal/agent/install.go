package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/middelkoopt/stage0/internal/qcow2"
)

// InstalledPrefix starts the line the agent prints after writing the disk.
const InstalledPrefix = "STAGE0-INSTALLED "

// Fetch downloads url to path (in the initramfs, so in RAM), printing
// progress. The checksum is checked later, by Install.
func Fetch(url, path string, progress io.Writer) (int64, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	w := &progressWriter{w: f, limit: 1 << 62, out: progress, every: 256 << 20, start: start, verb: "fetched"}
	n, err := io.Copy(w, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("fetch %s: %w", url, err)
	}
	if d := time.Since(start).Seconds(); d > 0 {
		fmt.Fprintf(progress, "stage0 agent: fetched %d MB in %.1f s (%.1f MB/s)\n", n>>20, d, float64(n)/d/1e6)
	}
	return n, nil
}

// Result is a finished install.
type Result struct {
	Disk     string   `json:"disk"`
	Bytes    int64    `json:"bytes"`
	Seconds  float64  `json:"seconds"`
	MBps     float64  `json:"MBps"`
	Checksum string   `json:"sha256"`
	Seeded   []string `json:"seeded,omitempty"` // first-boot files written to the boot partition
}

// Install checks the payload against wantSHA, then decompresses it onto
// device, reporting progress. Nothing is written unless the checksum
// matches. An image larger than the device fails once it reaches the end of
// the device; the disk is then partly overwritten.
func Install(payloadPath, device, wantSHA string, progress io.Writer) (Result, error) {
	res := Result{Disk: device, Checksum: wantSHA}
	got, err := payload(payloadPath)
	if err != nil {
		return res, err
	}
	if got == nil {
		return res, fmt.Errorf("no payload at %s", payloadPath)
	}
	if got.SHA256 != wantSHA {
		return res, fmt.Errorf("payload checksum %s does not match stage0.sha256=%s; not writing", got.SHA256, wantSHA)
	}

	src, err := os.Open(payloadPath)
	if err != nil {
		return res, err
	}
	defer src.Close()
	var magic [6]byte
	if _, err := src.ReadAt(magic[:], 0); err != nil {
		return res, fmt.Errorf("payload: %w", err)
	}
	if string(magic[:4]) == "QFI\xfb" {
		return installQcow2(src, device, res, progress)
	}
	img, err := xz.NewReader(bufio.NewReaderSize(src, 1<<20))
	if err != nil {
		return res, fmt.Errorf("payload is neither xz nor qcow2: %w", err)
	}

	dst, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return res, err
	}
	size, err := dst.Seek(0, io.SeekEnd)
	if err != nil {
		dst.Close()
		return res, err
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		dst.Close()
		return res, err
	}

	start := time.Now()
	w := &progressWriter{w: dst, limit: size, out: progress, every: 256 << 20, start: start, verb: "written"}
	n, err := io.Copy(w, img)
	res.Bytes = n
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	res.Seconds = time.Since(start).Seconds()
	if res.Seconds > 0 {
		res.MBps = float64(int(float64(n)/res.Seconds/1e5)) / 10
	}
	return res, err
}

// installQcow2 writes a qcow2 payload's data clusters to device (the
// payload's checksum is already checked).
func installQcow2(src *os.File, device string, res Result, progress io.Writer) (Result, error) {
	img, err := qcow2.Open(src)
	if err != nil {
		return res, err
	}
	dst, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return res, err
	}
	size, err := dst.Seek(0, io.SeekEnd)
	if err == nil && img.Size() > size {
		err = fmt.Errorf("image (%d bytes) is larger than the disk (%d bytes)", img.Size(), size)
	}
	if err != nil {
		dst.Close()
		return res, err
	}
	start := time.Now()
	fmt.Fprintf(progress, "stage0 agent: writing a %d MB qcow2 image (data clusters only)\n", img.Size()>>20)
	written, err := img.WriteSparse(dst)
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	res.Bytes = img.Size()
	res.Seconds = time.Since(start).Seconds()
	if res.Seconds > 0 {
		res.MBps = float64(int(float64(written)/res.Seconds/1e5)) / 10
	}
	return res, err
}

// progressWriter writes to w, refusing to pass limit, and prints a progress
// line every so many bytes.
type progressWriter struct {
	w     io.Writer
	limit int64
	out   io.Writer
	every int64
	start time.Time
	verb  string // "written", "fetched"
	n     int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if p.n+int64(len(b)) > p.limit {
		return 0, fmt.Errorf("image is larger than the disk (%d bytes)", p.limit)
	}
	n, err := p.w.Write(b)
	before := p.n
	p.n += int64(n)
	if p.n/p.every != before/p.every {
		fmt.Fprintf(p.out, "STAGE0-PROGRESS %d MB %s, %.0f s\n", p.n>>20, p.verb, time.Since(p.start).Seconds())
	}
	return n, err
}
