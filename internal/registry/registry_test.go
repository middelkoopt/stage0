package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	for _, tt := range []struct{ in, host, repo, ref string }{
		{"quay.io/org/rocky10-rpi:10", "quay.io", "org/rocky10-rpi", "10"},
		{"localhost:5000/stage0/rocky10-rpi", "localhost:5000", "stage0/rocky10-rpi", "latest"},
		{"docker.io/rockylinux/rockylinux:10", "registry-1.docker.io", "rockylinux/rockylinux", "10"},
		{"docker.io/alpine", "registry-1.docker.io", "library/alpine", "latest"},
		{"ghcr.io/a/b@sha256:" + strings.Repeat("ab", 32), "ghcr.io", "a/b", "sha256:" + strings.Repeat("ab", 32)},
	} {
		r, err := ParseRef(tt.in)
		if err != nil || r.Host != tt.host || r.Repo != tt.repo || r.Ref != tt.ref {
			t.Errorf("ParseRef(%q) = %+v, %v", tt.in, r, err)
		}
	}
	for _, bad := range []string{"rocky10-rpi", "quay.io/", "quay.io/a b:x"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q): want error", bad)
		}
	}
	if r, _ := ParseRef("localhost:5000/a/b"); r.scheme() != "http" {
		t.Error("localhost registries are plain HTTP")
	}
	if r, _ := ParseRef("quay.io/a/b"); r.scheme() != "https" {
		t.Error("others are HTTPS")
	}
}

// fakeRegistry serves one multi-arch image (an index with arm64 and amd64
// manifests), wanting a bearer token from its token endpoint.
type fakeRegistry struct {
	blobs     map[string][]byte // digest -> content
	index     string
	arm64     string // manifest digest
	blobGets  int
	tokenSeen bool
	tokenUser string // "user:password" the token service wants, if any
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, *httptest.Server) {
	f := &fakeRegistry{blobs: map[string][]byte{}}
	put := func(b []byte) string {
		s := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(s[:])
		f.blobs[d] = b
		return d
	}
	manifest := func(arch string) (string, int) {
		cfg := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","config":{"Labels":{"arch":%q}}}`, arch, arch))
		layer := []byte("LAYER-" + arch)
		m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": put(cfg), "size": len(cfg)},
			"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": put(layer), "size": len(layer)}}})
		return put(m), len(m)
	}
	armD, armN := manifest("arm64")
	amdD, amdN := manifest("amd64")
	f.arm64 = armD
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{
			map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": amdD, "size": amdN, "platform": map[string]string{"architecture": "amd64", "os": "linux"}},
			map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": armD, "size": armN, "platform": map[string]string{"architecture": "arm64", "os": "linux"}},
		}})
	f.index = put(idx)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if f.tokenUser != "" {
				if u, p, ok := r.BasicAuth(); !ok || u+":"+p != f.tokenUser {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			if r.URL.Query().Get("scope") != "repository:org/img:pull" {
				t.Errorf("token scope %q", r.URL.Query().Get("scope"))
			}
			fmt.Fprint(w, `{"token":"T0K3N"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer T0K3N" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:org/img:pull"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.tokenSeen = true
		switch {
		case r.URL.Path == "/v2/org/img/manifests/1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", f.index)
			w.Write(f.blobs[f.index])
		case strings.HasPrefix(r.URL.Path, "/v2/org/img/manifests/sha256:"):
			d := strings.TrimPrefix(r.URL.Path, "/v2/org/img/manifests/")
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Write(f.blobs[d])
		case strings.HasPrefix(r.URL.Path, "/v2/org/img/blobs/"):
			f.blobGets++
			b, ok := f.blobs[strings.TrimPrefix(r.URL.Path, "/v2/org/img/blobs/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	return f, srv
}

func TestPull(t *testing.T) {
	f, srv := newFakeRegistry(t)
	defer srv.Close()
	ref, err := ParseRef(strings.TrimPrefix(srv.URL, "http://") + "/org/img:1")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client()}
	cache := t.TempDir()
	dir, err := c.Pull(t.Context(), ref, "arm64", cache)
	if err != nil {
		t.Fatal(err)
	}
	if !f.tokenSeen {
		t.Error("no token used")
	}
	if dir != filepath.Join(cache, strings.TrimPrefix(f.arm64, "sha256:")) {
		t.Errorf("layout at %s, want one named by the arm64 manifest's digest", dir)
	}
	// An OCI layout of the arm64 image: index.json names its manifest.
	var idx struct{ Manifests []struct{ Digest string } }
	b, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if json.Unmarshal(b, &idx) != nil || len(idx.Manifests) != 1 || idx.Manifests[0].Digest != f.arm64 {
		t.Errorf("index.json %s", b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", hex8(t, "LAYER-arm64"))); err != nil || string(b) != "LAYER-arm64" {
		t.Errorf("layer: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", hex8(t, "LAYER-amd64"))); err == nil {
		t.Error("the amd64 layer was pulled too")
	}
	// Again: nothing new to download.
	gets := f.blobGets
	if _, err := c.Pull(t.Context(), ref, "arm64", cache); err != nil {
		t.Fatal(err)
	}
	if f.blobGets != gets {
		t.Errorf("pulled %d blobs again", f.blobGets-gets)
	}
	if _, err := c.Pull(t.Context(), ref, "riscv64", cache); err == nil {
		t.Error("no riscv64 image: want error")
	}
}

func TestPullBadBlob(t *testing.T) {
	f, srv := newFakeRegistry(t)
	defer srv.Close()
	for d, b := range f.blobs {
		if string(b) == "LAYER-arm64" {
			f.blobs[d] = []byte("TAMPERED")
		}
	}
	ref, _ := ParseRef(strings.TrimPrefix(srv.URL, "http://") + "/org/img:1")
	if _, err := (&Client{HTTP: srv.Client()}).Pull(t.Context(), ref, "arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("a tampered layer: err = %v", err)
	}
}

func hex8(t *testing.T, s string) string {
	t.Helper()
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestPullBasicAuth(t *testing.T) {
	// A registry:2 with htpasswd: Basic challenge, credentials on every request.
	f, _ := newFakeRegistry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "ci" || p != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Registry Realm"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/org/img/manifests/1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Write(f.blobs[f.index])
		case strings.HasPrefix(r.URL.Path, "/v2/org/img/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Write(f.blobs[strings.TrimPrefix(r.URL.Path, "/v2/org/img/manifests/")])
		case strings.HasPrefix(r.URL.Path, "/v2/org/img/blobs/"):
			w.Write(f.blobs[strings.TrimPrefix(r.URL.Path, "/v2/org/img/blobs/")])
		}
	}))
	defer srv.Close()
	ref, _ := ParseRef(strings.TrimPrefix(srv.URL, "http://") + "/org/img:1")
	if _, err := (&Client{}).Pull(t.Context(), ref, "arm64", t.TempDir()); err == nil {
		t.Error("no credentials: want error")
	}
	c := &Client{Credentials: map[string]Credentials{ref.Host: {User: "ci", Password: "pw"}}}
	if _, err := c.Pull(t.Context(), ref, "arm64", t.TempDir()); err != nil {
		t.Errorf("with credentials: %v", err)
	}
}

func TestPullTokenWithCredentials(t *testing.T) {
	// A private repository behind a token service: the token request
	// carries the credentials.
	f, srv := newFakeRegistry(t)
	defer srv.Close()
	f.tokenUser = "ci:pw"
	ref, _ := ParseRef(strings.TrimPrefix(srv.URL, "http://") + "/org/img:1")
	if _, err := (&Client{}).Pull(t.Context(), ref, "arm64", t.TempDir()); err == nil {
		t.Error("anonymous token for a private repository: want error")
	}
	c := &Client{Credentials: map[string]Credentials{ref.Host: {User: "ci", Password: "pw"}}}
	if _, err := c.Pull(t.Context(), ref, "arm64", t.TempDir()); err != nil {
		t.Errorf("with credentials: %v", err)
	}
}
