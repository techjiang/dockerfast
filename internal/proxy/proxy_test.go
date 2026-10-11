package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockerfast/dockerfast/internal/cache"
	"github.com/dockerfast/dockerfast/internal/config"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newTestRegistryCfg(t *testing.T, endpoint string, mutate func(*config.Config)) *Registry {
	t.Helper()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{
		{Name: "test.local", Hosts: []string{"test.local"}, Endpoint: endpoint, Default: true},
	}
	cfg.DataDir = t.TempDir()
	if mutate != nil {
		mutate(cfg)
	}
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

func newTestRegistry(t *testing.T, endpoint string, allowPush bool) *Registry {
	t.Helper()
	return newTestRegistryCfg(t, endpoint, func(c *config.Config) {
		c.AllowPush = allowPush
	})
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
	if got := resp.Header.Get(cacheStatusHeader); got != cacheMiss {
		t.Fatalf("first manifest cache header = %q, want %q", got, cacheMiss)
	}

	// Second manifest request: served from cache, upstream hit count unchanged.
	resp2, err := http.Get(front.URL + "/v2/library/nginx/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if got := resp2.Header.Get(cacheStatusHeader); got != cacheHit {
		t.Fatalf("second manifest cache header = %q, want %q", got, cacheHit)
	}
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
	if got := resp3.Header.Get(cacheStatusHeader); got != cacheMiss {
		t.Fatalf("first blob cache header = %q, want %q", got, cacheMiss)
	}
	resp4, err := http.Get(front.URL + "/v2/library/nginx/blobs/" + blobDigest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if got := resp4.Header.Get(cacheStatusHeader); got != cacheHit {
		t.Fatalf("second blob cache header = %q, want %q", got, cacheHit)
	}
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

func TestProxyAuth(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	reg := newTestRegistryCfg(t, upstream.URL, func(c *config.Config) {
		c.Auth = config.Auth{Enabled: true, Username: "admin", Password: "s3cret"}
	})
	front := httptest.NewServer(reg.Handler())
	defer front.Close()

	// No credentials -> 401 with a challenge.
	resp, err := http.Get(front.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("missing basic challenge: %q", resp.Header.Get("WWW-Authenticate"))
	}

	// Health check stays open.
	hresp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("healthz should be open, got %d", hresp.StatusCode)
	}

	// Correct credentials -> 200.
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v2/", nil)
	req.SetBasicAuth("admin", "s3cret")
	okResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("auth status = %d, want 200", okResp.StatusCode)
	}

	// Wrong password -> 401.
	badReq, _ := http.NewRequest(http.MethodGet, front.URL+"/v2/", nil)
	badReq.SetBasicAuth("admin", "wrong")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", badResp.StatusCode)
	}
}

func TestStatusAndDashboard(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", resp.StatusCode)
	}
	var st statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.Version == "" {
		t.Fatal("status version is empty")
	}
	if len(st.Upstreams) == 0 {
		t.Fatal("status should list upstreams")
	}
	if st.Cache.MaxBytes != 0 {
		// newTestRegistry uses a cache with no limit (0); field should be present.
		t.Logf("maxBytes = %d", st.Cache.MaxBytes)
	}
	if _, ok := st.Metrics["dockerfast_requests_total"]; !ok {
		t.Fatal("status metrics missing requests counter")
	}

	dresp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	dbody, _ := io.ReadAll(dresp.Body)
	dresp.Body.Close()
	if !strings.Contains(string(dbody), "DockerFast") {
		t.Fatal("dashboard HTML does not contain the title")
	}
	if ct := dresp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("dashboard content-type = %q", ct)
	}
}

func TestConcurrentBlobDedup(t *testing.T) {
	blobData := bytes.Repeat([]byte("xyzzy-"), 1<<14) // ~96 KiB
	blobDigest := sha256Digest(blobData)

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/library/nginx/blobs/") {
			hits.Add(1)
			// Hold the response briefly so concurrent requests overlap.
			time.Sleep(150 * time.Millisecond)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", blobDigest)
			_, _ = w.Write(blobData)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	front := httptest.NewServer(newTestRegistry(t, upstream.URL, false).Handler())
	defer front.Close()

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(front.URL + "/v2/library/nginx/blobs/" + blobDigest)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
				return
			}
			if !bytes.Equal(body, blobData) {
				errs <- fmt.Errorf("body mismatch, got %d bytes", len(body))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream blob hits = %d, want 1 (deduplication)", hits.Load())
	}
}
