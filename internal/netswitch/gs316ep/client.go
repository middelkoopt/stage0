// Package gs316ep drives a NETGEAR GS316EP through its web interface, which
// is the only management interface it has.
//
// Session protocol: the login page carries a nonce ("rand"); the browser
// posts MD5(password interleaved with nonce) and gets back a session token
// ("Gambit") that every later request carries. The switch allows few
// concurrent sessions, so every session is logged out when done.
package gs316ep

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/middelkoopt/stage0/internal/netswitch"
)

// Credentials authenticate to the switch. Set Password, or LoginHash for a
// precomputed login value (as stored by the older lab switch.py).
type Credentials struct {
	Password  string
	LoginHash string
}

// Client talks to one switch.
type Client struct {
	base *url.URL
	cred Credentials
	http *http.Client
}

var _ netswitch.Controller = (*Client)(nil)

// New returns a client for the switch at base, a URL or a bare host name.
// A nil httpClient means http.DefaultClient.
func New(base string, cred Credentials, httpClient *http.Client) (*Client, error) {
	if cred.Password == "" && cred.LoginHash == "" {
		return nil, errors.New("gs316ep: no password or login hash")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("gs316ep: switch address: %w", err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: u, cred: cred, http: httpClient}, nil
}

// Status logs in, reads the dashboard, PoE and VLAN pages, and logs out.
func (c *Client) Status(ctx context.Context) (st netswitch.Status, err error) {
	s, err := c.login(ctx)
	if err != nil {
		return st, err
	}
	defer func() {
		if lerr := s.logout(ctx); lerr != nil && err == nil {
			err = lerr
		}
	}()

	st, _, err = s.status(ctx)
	return st, err
}

// status reads and parses the three status pages, and returns the pages too
// for callers that need more from them.
func (s *session) status(ctx context.Context) (netswitch.Status, map[string][]byte, error) {
	pages := make(map[string][]byte)
	for _, name := range []string{"dashboard", "poePortConf", "vlan"} {
		var err error
		if pages[name], err = s.page(ctx, name); err != nil {
			return netswitch.Status{}, nil, err
		}
	}
	st, err := parseStatus(pages["dashboard"], pages["poePortConf"], pages["vlan"])
	if err != nil {
		return netswitch.Status{}, nil, fmt.Errorf("gs316ep: %w", err)
	}
	return st, pages, nil
}

// session is one logged-in session.
type session struct {
	c      *Client
	gambit string
}

func (c *Client) login(ctx context.Context) (*session, error) {
	page, err := c.do(ctx, http.MethodGet, "/", "")
	if err != nil {
		return nil, err
	}
	hash := c.cred.LoginHash
	if hash == "" {
		nonce, err := find(page, func(e element) (string, bool) {
			if e.attrs["id"] == "rand" {
				return e.attr("value")
			}
			return "", false
		})
		if err != nil {
			return nil, fmt.Errorf("gs316ep: login page: no nonce: %w", err)
		}
		hash = loginHash(c.cred.Password, nonce)
	}

	page, err = c.do(ctx, http.MethodPost, "/redirect.html", url.Values{"LoginPassword": {hash}}.Encode())
	if err != nil {
		return nil, err
	}
	gambit, err := find(page, func(e element) (string, bool) {
		if e.tag == "input" && e.attrs["name"] == "Gambit" {
			return e.attr("value")
		}
		return "", false
	})
	if err != nil || gambit == "" {
		return nil, errors.New("gs316ep: login rejected (wrong password, or too many sessions open)")
	}
	return &session{c: c, gambit: gambit}, nil
}

func (s *session) page(ctx context.Context, name string) ([]byte, error) {
	page, err := s.c.do(ctx, http.MethodGet, "/iss/specific/"+name+".html", url.Values{"Gambit": {s.gambit}}.Encode())
	if err != nil {
		return nil, err
	}
	if isLoginPage(page) {
		return nil, fmt.Errorf("gs316ep: session ended while reading %s page", name)
	}
	return page, nil
}

// post sends a change to one of the pages, with the session token last as
// switch.py sends it. The reply carries nothing the driver relies on; callers
// check the result by reading the switch back.
func (s *session) post(ctx context.Context, name string, f form) error {
	body := append(f[:len(f):len(f)], field{"Gambit", s.gambit})
	page, err := s.c.do(ctx, http.MethodPost, "/iss/specific/"+name+".html", body.encode())
	if err != nil {
		return err
	}
	if isLoginPage(page) {
		return fmt.Errorf("gs316ep: session ended while writing to the %s page", name)
	}
	return nil
}

func (s *session) logout(ctx context.Context) error {
	_, err := s.c.do(ctx, http.MethodGet, "/iss/specific/logout.html", url.Values{"Gambit": {s.gambit}}.Encode())
	return err
}

// do sends a request with an encoded form; for GET, it is the query string.
func (c *Client) do(ctx context.Context, method, path, form string) ([]byte, error) {
	u := c.base.JoinPath(path)
	var body io.Reader
	if method == http.MethodGet {
		u.RawQuery = form
	} else {
		body = strings.NewReader(form)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gs316ep: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gs316ep: %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gs316ep: %s %s: %s", method, path, resp.Status)
	}
	return b, nil
}

// loginHash is what the login page's JavaScript posts: MD5 of the password
// and nonce interleaved character by character.
func loginHash(password, nonce string) string {
	var b strings.Builder
	for i := range max(len(password), len(nonce)) {
		if i < len(password) {
			b.WriteByte(password[i])
		}
		if i < len(nonce) {
			b.WriteByte(nonce[i])
		}
	}
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func isLoginPage(page []byte) bool {
	_, err := find(page, func(e element) (string, bool) {
		return "", e.attrs["id"] == "rand"
	})
	return err == nil
}

// find returns the first value match reports.
func find(page []byte, match func(element) (string, bool)) (string, error) {
	elems, err := scan(page)
	if err != nil {
		return "", err
	}
	for _, e := range elems {
		if v, ok := match(e); ok {
			return v, nil
		}
	}
	return "", errors.New("not found")
}
