// Package registry pulls a container image from an OCI registry (the OCI
// distribution API: Docker Hub, quay.io, ghcr.io, a local registry) into
// an OCI image layout directory, the same as `podman save --format
// oci-dir` writes, so stage0 can serve it to nodes that never reach the
// registry themselves. Pulls are anonymous, with the bearer token most
// public registries ask for; every blob is checked against its digest.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Ref is an image reference: host/repo:tag or host/repo@digest.
type Ref struct {
	Host string // registry host[:port]; Docker Hub is registry-1.docker.io
	Repo string // e.g. rockylinux/rockylinux
	Ref  string // a tag, or a digest "sha256:…"
}

var (
	repoName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	tagName  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digest   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ParseRef reads a reference with its registry host (one with a dot or a
// port, or localhost), e.g. quay.io/org/img:tag. Docker Hub is docker.io;
// its one-part names are under library/.
func ParseRef(s string) (Ref, error) {
	var r Ref
	host, rest, ok := strings.Cut(s, "/")
	if !ok || !(strings.ContainsAny(host, ".:") || host == "localhost") {
		return r, fmt.Errorf("image reference %q: want registry/repository[:tag], e.g. quay.io/org/image:tag", s)
	}
	repo, ref := rest, "latest"
	if i := strings.Index(rest, "@"); i >= 0 {
		repo, ref = rest[:i], rest[i+1:]
		if !digest.MatchString(ref) {
			return r, fmt.Errorf("image reference %q: digest %q is not sha256:<64 hex>", s, ref)
		}
	} else if i := strings.LastIndex(rest, ":"); i >= 0 {
		repo, ref = rest[:i], rest[i+1:]
		if !tagName.MatchString(ref) {
			return r, fmt.Errorf("image reference %q: bad tag %q", s, ref)
		}
	}
	if !repoName.MatchString(repo) {
		return r, fmt.Errorf("image reference %q: bad repository %q", s, repo)
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	return Ref{Host: host, Repo: repo, Ref: ref}, nil
}

func (r Ref) String() string {
	sep := ":"
	if digest.MatchString(r.Ref) {
		sep = "@"
	}
	return r.Host + "/" + r.Repo + sep + r.Ref
}

// scheme is plain HTTP for a registry on localhost, as podman does.
func (r Ref) scheme() string {
	h := r.Host
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	if h == "localhost" || h == "127.0.0.1" || h == "[::1]" {
		return "http"
	}
	return "https"
}

// Client pulls images.
type Client struct {
	HTTP  *http.Client // default http.DefaultClient
	token string
}

const (
	ociIndex      = "application/vnd.oci.image.index.v1+json"
	ociManifest   = "application/vnd.oci.image.manifest.v1+json"
	dockerList    = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerSchema2 = "application/vnd.docker.distribution.manifest.v2+json"
)

type descriptor struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}

// Pull fetches the image ref names for linux/arch (an index's matching
// manifest; a single manifest must be for arch) into
// <cacheDir>/<manifest digest hex>, an OCI layout, and returns that
// directory. Blobs already there aren't fetched again.
func (c *Client) Pull(ctx context.Context, ref Ref, arch, cacheDir string) (string, error) {
	body, mediaType, err := c.get(ctx, ref, "manifests/"+ref.Ref, ociIndex+","+ociManifest+","+dockerList+","+dockerSchema2)
	if err != nil {
		return "", err
	}
	desc := descriptor{MediaType: mediaType, Digest: sum(body), Size: int64(len(body))}
	if digest.MatchString(ref.Ref) && desc.Digest != ref.Ref {
		return "", fmt.Errorf("%s: manifest digest is %s", ref, desc.Digest)
	}
	if mediaType == ociIndex || mediaType == dockerList {
		var idx struct{ Manifests []descriptor }
		if err := json.Unmarshal(body, &idx); err != nil {
			return "", fmt.Errorf("%s: index: %w", ref, err)
		}
		var found *descriptor
		for i, m := range idx.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == arch {
				found = &idx.Manifests[i]
				break
			}
		}
		if found == nil {
			return "", fmt.Errorf("%s: no linux/%s image", ref, arch)
		}
		if body, mediaType, err = c.get(ctx, ref, "manifests/"+found.Digest, ociManifest+","+dockerSchema2); err != nil {
			return "", err
		}
		if sum(body) != found.Digest {
			return "", fmt.Errorf("%s: manifest %s: digest is %s", ref, found.Digest, sum(body))
		}
		desc = descriptor{MediaType: mediaType, Digest: found.Digest, Size: int64(len(body))}
	}
	var m struct {
		Config descriptor
		Layers []descriptor
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("%s: manifest: %w", ref, err)
	}

	dir := filepath.Join(cacheDir, strings.TrimPrefix(desc.Digest, "sha256:"))
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return "", err
	}
	for _, b := range append([]descriptor{m.Config}, m.Layers...) {
		if err := c.blob(ctx, ref, b.Digest, blobs); err != nil {
			return "", err
		}
	}
	if err := writeFile(filepath.Join(blobs, strings.TrimPrefix(desc.Digest, "sha256:")), body); err != nil {
		return "", err
	}
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []descriptor{desc}})
	if err := writeFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return "", err
	}
	// Last: a layout with index.json is complete.
	return dir, writeFile(filepath.Join(dir, "index.json"), idx)
}

// blob fetches one blob into dir unless it is there, checking its digest.
func (c *Client) blob(ctx context.Context, ref Ref, d, dir string) error {
	if !digest.MatchString(d) {
		return fmt.Errorf("%s: blob digest %q: only sha256 is supported", ref, d)
	}
	dst := filepath.Join(dir, strings.TrimPrefix(d, "sha256:"))
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	resp, err := c.do(ctx, ref, "blobs/"+d, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp(dir, ".blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if err := errors.Join(err, tmp.Close()); err != nil {
		return fmt.Errorf("%s: blob %s: %w", ref, d, err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d {
		return fmt.Errorf("%s: blob %s: digest is %s", ref, d, got)
	}
	return os.Rename(tmp.Name(), dst)
}

// get fetches a small document (a manifest), with its media type.
func (c *Client) get(ctx context.Context, ref Ref, path, accept string) ([]byte, string, error) {
	resp, err := c.do(ctx, ref, path, accept)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return b, mt, nil
}

// do GETs /v2/<repo>/<path>, getting an anonymous bearer token first if
// the registry asks for one.
func (c *Client) do(ctx context.Context, ref Ref, path, accept string) (*http.Response, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	u := ref.scheme() + "://" + ref.Host + "/v2/" + ref.Repo + "/" + path
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if c.token, err = c.login(ctx, hc, challenge); err != nil {
				return nil, fmt.Errorf("%s: %w", ref, err)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s: %s: %s", ref, path, resp.Status)
		}
		return resp, nil
	}
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// login gets an anonymous token from the realm a Bearer challenge names.
func (c *Client) login(ctx context.Context, hc *http.Client, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry wants %q authentication; only anonymous bearer tokens are supported", scheme)
	}
	p := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(params, -1) {
		p[m[1]] = m[2]
	}
	if p["realm"] == "" {
		return "", errors.New("bearer challenge without a realm")
	}
	q := url.Values{}
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token: %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func writeFile(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
