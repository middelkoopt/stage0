package cli

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/middelkoopt/stage0/internal/agent"
)

// Console events install waits for.
const (
	errorPrefix = "STAGE0-ERROR "
	readyEvent  = "ready"
)

// takeReport reports whether a report event counts: valid JSON, and not
// a console copy once an HTTP one has come (HTTP is the authority).
func takeReport(e event, haveHTTP bool) bool {
	return json.Valid([]byte(e.text)) && (e.http || !haveHTTP)
}

type event struct {
	kind, text string
	http       bool // from the agent's HTTP report, not the console
}

// watcher scans console output for the agent's lines and, once armed, the
// installed system's ready pattern.
type watcher struct {
	ctx    context.Context
	ready  *regexp.Regexp
	events chan event

	mu    sync.Mutex
	buf   []byte
	armed bool
}

func newWatcher(ctx context.Context, ready *regexp.Regexp) *watcher {
	return &watcher{ctx: ctx, ready: ready, events: make(chan event, 16)}
}

// send delivers an event unless the watcher is done.
func (w *watcher) send(e event) {
	select {
	case w.events <- e:
	case <-w.ctx.Done():
	}
}

// armReady starts looking for the ready pattern, in output from now on.
func (w *watcher) armReady() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed, w.buf = true, nil
}

func (w *watcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	var found []event
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		for _, k := range []string{agent.ReportPrefix, agent.InstalledPrefix, errorPrefix} {
			if j := strings.Index(line, k); j >= 0 {
				found = append(found, event{kind: k, text: line[j+len(k):]})
			}
		}
		if w.armed {
			if m := w.ready.FindString(line); m != "" {
				found = append(found, event{kind: readyEvent, text: m})
			}
		}
	}
	// A prompt has no newline after it.
	if w.armed {
		if m := w.ready.Find(w.buf); m != nil {
			found = append(found, event{kind: readyEvent, text: string(m)})
			w.buf = nil
		}
	}
	if len(w.buf) > 64<<10 {
		w.buf = w.buf[len(w.buf)-64<<10:]
	}
	w.mu.Unlock()
	for _, e := range found {
		w.send(e)
	}
	return len(p), nil
}
