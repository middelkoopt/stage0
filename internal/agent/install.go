package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// InstalledPrefix starts the line the agent prints after writing the disk.
const InstalledPrefix = "STAGE0-INSTALLED "

// Plan is an install the kernel command line asks for. The agent writes a
// disk only when the command line names both the disk and the payload's
// checksum, so booting the agent never wipes a disk by accident.
type Plan struct {
	Disk   string // name under /dev, e.g. mmcblk0
	SHA256 string // of the payload, an xz-compressed raw disk image
}

var (
	diskName = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	hexSum   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParsePlan reads stage0.disk= and stage0.sha256= from a kernel command
// line; nil when neither is there.
func ParsePlan(cmdline string) (*Plan, error) {
	var p Plan
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, "stage0.disk="); ok {
			p.Disk = v
		}
		if v, ok := strings.CutPrefix(f, "stage0.sha256="); ok {
			p.SHA256 = v
		}
	}
	switch {
	case p.Disk == "" && p.SHA256 == "":
		return nil, nil
	case p.Disk == "":
		return nil, errors.New("stage0.sha256 is set but stage0.disk is not")
	case p.SHA256 == "":
		return nil, errors.New("stage0.disk is set but stage0.sha256 is not")
	case !diskName.MatchString(p.Disk):
		return nil, fmt.Errorf("stage0.disk=%q is not a disk name like mmcblk0", p.Disk)
	case !hexSum.MatchString(p.SHA256):
		return nil, fmt.Errorf("stage0.sha256=%q is not 64 hex digits", p.SHA256)
	}
	return &p, nil
}

// Result is a finished install.
type Result struct {
	Disk     string  `json:"disk"`
	Bytes    int64   `json:"bytes"`
	Seconds  float64 `json:"seconds"`
	MBps     float64 `json:"MBps"`
	Checksum string  `json:"sha256"`
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
	img, err := xz.NewReader(bufio.NewReaderSize(src, 1<<20))
	if err != nil {
		return res, fmt.Errorf("payload is not xz: %w", err)
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
	w := &progressWriter{w: dst, limit: size, out: progress, every: 256 << 20, start: start}
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

// progressWriter writes to w, refusing to pass limit, and prints a progress
// line every so many bytes.
type progressWriter struct {
	w     io.Writer
	limit int64
	out   io.Writer
	every int64
	start time.Time
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
		fmt.Fprintf(p.out, "STAGE0-PROGRESS %d MB written, %.0f s\n", p.n>>20, time.Since(p.start).Seconds())
	}
	return n, err
}
