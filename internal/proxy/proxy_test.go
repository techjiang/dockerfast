package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dockerfast/dockerfast/internal/cache"
	"github.com/dockerfast/dockerfast/internal/config"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newTestRegistry(t *testing.T, endpoint string, allowPush bool) *Registry {
	t.Helper()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{
		{Name: "test.local", Hosts: []string{"test.local"}, Endpoint: endpoint, Default: true},
	}
	cfg.AllowPush = allowPush
	cfg.DataDir = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := New(cfg, c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestManifestAndBlobCaching(t *testing.T) {
	manifestBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	manifestDigest := sha256Digest(manifestBody)
	blobData := []byte("0123456789-layer-bytes")
	blobDigest := sha256Digest(blobData)

	var manifestHits, blobHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/library/nginx/manifests/"):
			manifestHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestBody)
		case strings.HasPrefix(r.URL.Path, "/v2/library/nginx/blobs/"):
			blobHits.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", blobDigest)
			_, _ = w.Write(blobData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	// First manifest request: fetched from upstream.
	resp, err := http.Get(front.URL + "/v2/library/nginx/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d", resp.StatusCode)
	}
	if string(body) != string(manifestBody) {
		t.Fatalf("manifest body mismatch: %s", body)
	}
	if resp.Header.Get("Docker-Content-Digest") != manifestDigest {
		t.Fatalf("digest header = %q", resp.Header.Get("Docker-Content-Digest"))
	}

	// Second manifest request: served from cache, upstream hit count unchanged.
	resp2, err := http.Get(front.URL + "/v2/library/nginx/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if manifestHits.Load() != 1 {
		t.Fatalf("manifest upstream hits = %d, want 1", manifestHits.Load())
	}

	// Blob: fetch then cache.
	resp3, err := http.Get(front.URL + "/v2/library/nginx/blobs/" + blobDigest)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if string(blob) != string(blobData) {
		t.Fatalf("blob body mismatch: %q", blob)
	}
	resp4, err := http.Get(front.URL + "/v2/library/nginx/blobs/" + blobDigest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if blobHits.Load() != 1 {
		t.Fatalf("blob upstream hits = %d, want 1", blobHits.Load())
	}
}

func TestUpstreamBearerAuth(t *testing.T) {
	manifestBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	manifestDigest := sha256Digest(manifestBody)

	var baseURL string
	var tokenHits, manifestHits atomic.Int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			tokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"testtoken","expires_in":300}`)
		case strings.HasPrefix(r.URL.Path, "/v2/private/repo/manifests/"):
			if r.Header.Get("Authorization") != "Bearer testtoken" {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+baseURL+`/token",service="test",scope="repository:private/repo:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			manifestHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestBody)
		default:
			http.NotFound(w, r)
		}
	})
	upstream := httptest.NewServer(handler)
	defer upstream.Close()
	baseURL = upstream.URL

	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/v2/private/repo/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if string(body) != string(manifestBody) {
		t.Fatalf("body mismatch: %s", body)
	}
	if tokenHits.Load() != 1 {
		t.Fatalf("token hits = %d, want 1", tokenHits.Load())
	}
	if manifestHits.Load() != 1 {
		t.Fatalf("manifest hits = %d, want 1", manifestHits.Load())
	}
}

func TestPushDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be contacted for a disabled push")
	}))
	defer upstream.Close()

	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	resp, err := http.Post(front.URL+"/v2/library/nginx/blobs/uploads/", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("push status = %d, want 405", resp.StatusCode)
	}
}

func TestUpstreamNotFoundMapsTo404(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	missingBlob := "sha256:" + strings.Repeat("a", 64)
	resp, err := http.Get(front.URL + "/v2/library/nginx/blobs/" + missingBlob)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing blob status = %d, want 404", resp.StatusCode)
	}

	mresp, err := http.Get(front.URL + "/v2/library/nginx/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing manifest status = %d, want 404", mresp.StatusCode)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	mresp, err := http.Get(front.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	mbody, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if !strings.Contains(string(mbody), "dockerfast_requests_total") {
		t.Fatalf("metrics missing counter: %s", mbody)
	}
}
