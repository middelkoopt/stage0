package agent

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// What the agent tells stage0: on the console, as lines stage0 console
// --until can wait for, and, with stage0.report=<url> on the command line,
// by HTTP POST to <url>/<kind>, for nodes without a serial console.
const (
	kindReport    = "report"    // STAGE0-REPORT {json}
	kindInstalled = "installed" // STAGE0-INSTALLED {json}
	kindError     = "error"     // STAGE0-ERROR message
)

var prefixes = map[string]string{kindReport: ReportPrefix, kindInstalled: InstalledPrefix, kindError: ErrorPrefix}

// ErrorPrefix starts the line the agent prints when it fails.
const ErrorPrefix = "STAGE0-ERROR "

type emitter struct {
	out io.Writer
	url string // without the kind; empty: console only
}

// emit prints one line and, if there is a report URL, posts it. A post
// that fails is noted on the console: the console line still stands.
func (e emitter) emit(kind string, body []byte) {
	fmt.Fprintf(e.out, "%s%s\n", prefixes[kind], body)
	if e.url == "" {
		return
	}
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Post(e.url+"/"+kind, "application/octet-stream", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			err = fmt.Errorf("%s", resp.Status)
		}
	}
	if err != nil {
		fmt.Fprintf(e.out, "stage0 agent: report not delivered to %s: %v\n", e.url, err)
	}
}

// reportURL reads stage0.report= from a kernel command line.
func reportURL(cmdline string) string {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, "stage0.report="); ok && strings.HasPrefix(v, "http://") {
			return v
		}
	}
	return ""
}
