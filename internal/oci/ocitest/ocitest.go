// Package ocitest writes OCI image layouts for tests.
package ocitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type descriptor struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Write writes an OCI image layout to dir: one manifest, its config with
// labels (architecture arm64), and one gzip tar layer per map of files (a
// nil body is a directory; entries in sorted order). It returns the
// manifest's digest.
func Write(t *testing.T, dir string, labels map[string]string, layers ...map[string][]byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	blob := func(mediaType string, b []byte) descriptor {
		sum := sha256.Sum256(b)
		d := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", d), b, 0o644); err != nil {
			t.Fatal(err)
		}
		return descriptor{MediaType: mediaType, Digest: "sha256:" + d, Size: int64(len(b))}
	}
	var ls []descriptor
	for _, files := range layers {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		tw := tar.NewWriter(z)
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, name := range names {
			body := files[name]
			if body == nil {
				tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
				continue
			}
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
			tw.Write(body)
		}
		tw.Close()
		z.Close()
		ls = append(ls, blob("application/vnd.oci.image.layer.v1.tar+gzip", buf.Bytes()))
	}
	cfg, _ := json.Marshal(map[string]any{"architecture": "arm64", "os": "linux", "config": map[string]any{"Labels": labels}})
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": blob("application/vnd.oci.image.config.v1+json", cfg), "layers": ls})
	md := blob("application/vnd.oci.image.manifest.v1+json", m)
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []descriptor{md}})
	os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o644)
	os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
	return md.Digest
}
