package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func digestOf(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func newTestCache(t *testing.T, max int64) *Cache {
	t.Helper()
	c, err := New(t.TempDir(), max, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseDigest(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	if _, err := ParseDigest(valid); err != nil {
		t.Fatalf("valid digest rejected: %v", err)
	}
	for _, bad := range []string{"", "sha256", "sha256:", "sha256:xyz", strings.Repeat("a", 63), "md5:" + strings.Repeat("a", 32)} {
		if _, err := ParseDigest(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestBlobRoundTrip(t *testing.T) {
	c := newTestCache(t, 0)
	data := []byte("hello layer")
	d := digestOf(data)

	if c.HasBlob(d) {
		t.Fatal("blob should not exist yet")
	}
	n, err := c.PutBlob(d, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) {
		t.Fatalf("wrote %d bytes, want %d", n, len(data))
	}
	if !c.HasBlob(d) {
		t.Fatal("blob should exist after put")
	}
	f, size, err := c.OpenBlob(d)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %q", got)
	}
	if c.TotalBytes() != int64(len(data)) {
		t.Fatalf("total bytes = %d", c.TotalBytes())
	}
}

func TestManifestAndRef(t *testing.T) {
	c := newTestCache(t, 0)
	data := []byte(`{"schemaVersion":2}`)
	d := digestOf(data)
	if err := c.PutManifest(d, "application/vnd.docker.distribution.manifest.v2+json", data); err != nil {
		t.Fatal(err)
	}
	got, media, ok := c.GetManifest(d)
	if !ok || !bytes.Equal(got, data) {
		t.Fatal("manifest round trip failed")
	}
	if media != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Fatalf("media = %q", media)
	}

	if err := c.PutRef("docker.io", "library/nginx", "latest", d.String()); err != nil {
		t.Fatal(err)
	}
	ref, ok := c.GetRef("docker.io", "library/nginx", "latest")
	if !ok || ref != d.String() {
		t.Fatalf("ref = %q, ok=%v", ref, ok)
	}
	if _, ok := c.GetRef("docker.io", "library/nginx", "missing"); ok {
		t.Fatal("unexpected ref hit")
	}
}

func TestRefTraversalSanitised(t *testing.T) {
	c := newTestCache(t, 0)
	// A hostile ref must not escape the cache root.
	if err := c.PutRef("docker.io", "../../etc", "../../passwd", "sha256:"+strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	got, ok := c.GetRef("docker.io", "../../etc", "../../passwd")
	if !ok || got == "" {
		t.Fatal("sanitised ref should still round-trip within root")
	}
	// The file must live under the refs directory.
	if !strings.Contains(c.refPath("docker.io", "../../etc", "../../passwd"), "refs") {
		t.Fatal("ref path escaped refs dir")
	}
}

func TestEviction(t *testing.T) {
	c := newTestCache(t, 1000)
	// Write 5 blobs of 400 bytes each (2000 total), forcing eviction.
	makeBlob := func(i byte) Digest {
		data := bytes.Repeat([]byte{i}, 400)
		d := digestOf(data)
		if _, err := c.PutBlob(d, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		// Ensure distinct mtimes so LRU order is deterministic.
		time.Sleep(2 * time.Millisecond)
		return d
	}
	var digests []Digest
	for i := byte(1); i <= 5; i++ {
		digests = append(digests, makeBlob(i))
	}
	if c.TotalBytes() > 1000 {
		t.Fatalf("cache not evicted: %d bytes", c.TotalBytes())
	}
	// The most recent blob must survive.
	if !c.HasBlob(digests[4]) {
		t.Fatal("most recently written blob should be retained")
	}
}

func TestCleanerExpiry(t *testing.T) {
	c := newTestCache(t, 0)
	data := []byte("expire me")
	d := digestOf(data)
	if _, err := c.PutBlob(d, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Backdate the file so it is older than the TTL.
	c.clean(0, 0) // no-op should not remove
	if !c.HasBlob(d) {
		t.Fatal("clean with zero TTL must not remove files")
	}
	c.clean(time.Nanosecond, time.Nanosecond)
	if c.HasBlob(d) {
		t.Fatal("expired blob should be removed")
	}
}
