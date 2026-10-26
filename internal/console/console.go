// Package console captures a node's serial console: it copies everything the
// node prints to the screen and a log, and can stop when a pattern appears
// (e.g. "login: "), so an agent can wait for a boot to finish.
package console

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	"go.bug.st/serial"
)

// tail is how much recent output a pattern is matched against.
const tail = 64 << 10

// readTimeout bounds each serial read, so Capture notices a cancelled
// context without the port having to close.
const readTimeout = 200 * time.Millisecond

// Open opens a serial console at baud, 8N1, with the read timeout Capture expects.
func Open(device string, baud int) (io.ReadCloser, error) {
	p, err := serial.Open(device, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("console %s: %w", device, err)
	}
	if err := p.SetReadTimeout(readTimeout); err != nil {
		p.Close()
		return nil, fmt.Errorf("console %s: %w", device, err)
	}
	return p, nil
}

// Capture copies src to dst until src ends or ctx is done. With until set,
// it also stops at the first match in the recent output and returns the
// matched text; ending without a match is then an error. A match that runs
// to the end of the output so far might still grow (a long line arriving in
// pieces), so it is taken once more output follows it, the port goes quiet
// (a read that returns no data, i.e. a serial read timeout), or the output
// ends.
func Capture(ctx context.Context, src io.Reader, dst io.Writer, until *regexp.Regexp) (string, error) {
	buf := make([]byte, 4096)
	var recent []byte
	pending := false // a match touches the end of recent
	match := func() string { return string(until.Find(recent)) }
	for {
		if err := ctx.Err(); err != nil {
			if pending {
				return match(), nil
			}
			if until != nil {
				return "", fmt.Errorf("stopped (time limit or interrupt) with no match for %q", until)
			}
			return "", nil
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return "", werr
			}
			if until != nil {
				recent = append(recent, buf[:n]...)
				if len(recent) > tail {
					recent = recent[len(recent)-tail:]
				}
				if loc := until.FindIndex(recent); loc != nil {
					if loc[1] < len(recent) {
						return string(recent[loc[0]:loc[1]]), nil
					}
					pending = true
				}
			}
		} else if pending && err == nil {
			return match(), nil // quiet: the match is complete
		}
		if err == io.EOF {
			if pending {
				return match(), nil
			}
			if until != nil {
				return "", fmt.Errorf("console output ended with no match for %q", until)
			}
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}
