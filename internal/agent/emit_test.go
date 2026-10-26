package agent

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestEmit(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	var out bytes.Buffer
	e := emitter{out: &out, url: srv.URL + "/stage0/report"}
	e.emit(kindReport, []byte(`{"model":"Pi"}`))
	e.emit(kindInstalled, []byte(`{"bytes":1}`))
	e.emit(kindError, []byte("install: no disk"))

	if s := out.String(); !strings.Contains(s, ReportPrefix+`{"model":"Pi"}`+"\n") ||
		!strings.Contains(s, InstalledPrefix+`{"bytes":1}`+"\n") || !strings.Contains(s, "STAGE0-ERROR install: no disk\n") {
		t.Errorf("console:\n%s", s)
	}
	want := map[string]string{
		"POST /stage0/report/report":    `{"model":"Pi"}`,
		"POST /stage0/report/installed": `{"bytes":1}`,
		"POST /stage0/report/error":     "install: no disk",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestEmitConsoleOnly(t *testing.T) {
	var out bytes.Buffer
	emitter{out: &out}.emit(kindError, []byte("x"))
	if out.String() != "STAGE0-ERROR x\n" {
		t.Errorf("%q", out.String())
	}
}

func TestEmitUnreachable(t *testing.T) {
	// A report that can't be delivered is noted on the console, not fatal.
	var out bytes.Buffer
	emitter{out: &out, url: "http://127.0.0.1:1/stage0/report"}.emit(kindReport, []byte("{}"))
	if !strings.Contains(out.String(), ReportPrefix+"{}") || !strings.Contains(out.String(), "stage0 agent: report not delivered") {
		t.Errorf("%q", out.String())
	}
}

func TestReportURL(t *testing.T) {
	if u := reportURL("console=x stage0.report=http://10.0.3.1/stage0/report y"); u != "http://10.0.3.1/stage0/report" {
		t.Errorf("url %q", u)
	}
	if u := reportURL("stage0.report=ftp://x"); u != "" {
		t.Errorf("non-http accepted: %q", u)
	}
}
