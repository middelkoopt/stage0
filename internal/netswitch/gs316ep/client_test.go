package gs316ep

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeSwitch serves the captured pages and checks the session protocol:
// login with the right hash, the Gambit token on every page, and a logout.
type fakeSwitch struct {
	t              *testing.T
	wantHash       string
	dropAfterLogin bool // serve the login page instead of status pages

	mu      sync.Mutex
	logins  int
	logouts int
	pages   []string
}

func (f *fakeSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/":
		w.Write(fixture(f.t, "login.html"))
	case r.URL.Path == "/redirect.html" && r.Method == http.MethodPost:
		if r.PostFormValue("LoginPassword") != f.wantHash {
			w.Write(fixture(f.t, "login.html")) // the switch re-serves the login page
			return
		}
		f.logins++
		w.Write(fixture(f.t, "redirect.html"))
	case r.URL.Path == "/iss/specific/logout.html":
		f.logouts++
	case strings.HasPrefix(r.URL.Path, "/iss/specific/"):
		if r.URL.Query().Get("Gambit") != "TESTGAMBIT" || f.dropAfterLogin {
			w.Write(fixture(f.t, "login.html")) // session lost: login page again
			return
		}
		page := strings.TrimPrefix(r.URL.Path, "/iss/specific/")
		b, err := os.ReadFile(filepath.Join("testdata", page))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f.pages = append(f.pages, page)
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func newFake(t *testing.T) (*fakeSwitch, *httptest.Server) {
	f := &fakeSwitch{t: t, wantHash: "689b7fd6ff2ba66440c3122c5eba39f1"} // loginHash("secret", "123456789")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestLoginHash(t *testing.T) {
	// Interleaves password and nonce, then MD5, as the switch's login page does.
	if got := loginHash("secret", "123456789"); got != "689b7fd6ff2ba66440c3122c5eba39f1" {
		t.Errorf("loginHash = %s", got)
	}
}

func TestStatus(t *testing.T) {
	f, srv := newFake(t)
	c, err := New(srv.URL, Credentials{Password: "secret"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := st.Port("k53"); !ok || p.ID != 2 || !p.Linked {
		t.Errorf("port k53 = %+v, %v", p, ok)
	}
	if f.logins != 1 || f.logouts != 1 {
		t.Errorf("logins=%d logouts=%d, want 1 and 1", f.logins, f.logouts)
	}
}

func TestStatusWithLoginHash(t *testing.T) {
	f, srv := newFake(t)
	c, err := New(srv.URL, Credentials{LoginHash: f.wantHash}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStatusWrongPassword(t *testing.T) {
	f, srv := newFake(t)
	c, err := New(srv.URL, Credentials{Password: "wrong"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("err = %v, want a login error", err)
	}
	if f.logouts != 0 {
		t.Errorf("logged out %d times without a session", f.logouts)
	}
}

func TestStatusLogsOutOnError(t *testing.T) {
	f, srv := newFake(t)
	c, err := New(srv.URL, Credentials{Password: "secret"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.dropAfterLogin = true // the switch drops the session: pages come back as the login page
	_, err = c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("err = %v, want a session error", err)
	}
	if f.logouts != 1 {
		t.Errorf("logouts = %d, want 1", f.logouts)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New("http://switch.example", Credentials{}, nil); err == nil {
		t.Error("New without credentials: want error")
	}
}

func TestNewAcceptsHostOnly(t *testing.T) {
	c, err := New("switch.example", Credentials{Password: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.base.String() != "http://switch.example" {
		t.Errorf("base = %s", c.base)
	}
}
