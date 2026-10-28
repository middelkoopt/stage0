package gs316ep

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeSwitch serves the captured pages and checks the session protocol:
// login with the right hash, the Gambit token on every request, and a logout.
// Posts are recorded; onPost may edit the served pages to apply them.
type fakeSwitch struct {
	t              *testing.T
	wantHash       string
	dropAfterLogin bool // serve the login page instead of status pages
	dropOnPost     bool // answer posts with the login page
	onPost         func(page string, form url.Values)

	mu      sync.Mutex
	logins  int
	logouts int
	served  map[string][]byte // page name -> current content, loaded on first use
	posts   []fakePost
}

type fakePost struct {
	page string
	body string // as sent: the real switch ignores forms whose fields are out of order
	form url.Values
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
	case strings.HasPrefix(r.URL.Path, "/iss/specific/") && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil || form.Get("Gambit") != "TESTGAMBIT" || f.dropOnPost {
			w.Write(fixture(f.t, "login.html"))
			return
		}
		page := strings.TrimPrefix(r.URL.Path, "/iss/specific/")
		f.posts = append(f.posts, fakePost{page, string(body), form})
		if f.onPost != nil {
			f.onPost(page, form)
		}
	case strings.HasPrefix(r.URL.Path, "/iss/specific/"):
		if r.URL.Query().Get("Gambit") != "TESTGAMBIT" || f.dropAfterLogin {
			w.Write(fixture(f.t, "login.html")) // session lost: login page again
			return
		}
		b, ok := f.page(strings.TrimPrefix(r.URL.Path, "/iss/specific/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// page returns a page's current content. Callers hold f.mu.
func (f *fakeSwitch) page(name string) ([]byte, bool) {
	if b, ok := f.served[name]; ok {
		return b, true
	}
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		return nil, false
	}
	if f.served == nil {
		f.served = make(map[string][]byte)
	}
	f.served[name] = b
	return b, true
}

// edit replaces the first old after anchor in a served page. It runs inside
// onPost (f.mu held), so it reports problems with Errorf, not Fatal.
func (f *fakeSwitch) edit(name, anchor, old, new string) {
	b, _ := f.page(name)
	s := string(b)
	a := strings.Index(s, anchor)
	if a < 0 {
		f.t.Errorf("fake: anchor %q not in %s", anchor, name)
		return
	}
	i := strings.Index(s[a:], old)
	if i < 0 {
		f.t.Errorf("fake: %q not after %q in %s", old, anchor, name)
		return
	}
	f.served[name] = []byte(s[:a+i] + new + s[a+i+len(old):])
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
