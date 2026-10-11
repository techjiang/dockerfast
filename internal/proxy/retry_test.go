package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRetryClient(t *testing.T, base http.RoundTripper, attempts int) *http.Client {
	t.Helper()
	tr := newRetryTransport(base, attempts, time.Millisecond, 2*time.Millisecond)
	tr.rand = func() float64 { return 0 } // deterministic, no jitter
	return &http.Client{Transport: tr}
}

func TestRetryTransportRetriesTransient(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	client := newTestRetryClient(t, upstream.Client().Transport, 5)
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream hits = %d, want 3", hits.Load())
	}
}

func TestRetryTransportExhaustsAndReturnsLast(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	client := newTestRetryClient(t, upstream.Client().Transport, 3)
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("final status = %d, want 503", resp.StatusCode)
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream hits = %d, want 3", hits.Load())
	}
}

func TestRetryTransportDoesNotRetryNonRetryable(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	client := newTestRetryClient(t, upstream.Client().Transport, 5)
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("non-retryable status should not be retried, hits = %d", hits.Load())
	}
}

func TestRetryTransportSkipsNonReplayableBody(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	client := newTestRetryClient(t, upstream.Client().Transport, 5)
	// A body that is not a known rewindable type -> GetBody is nil.
	req, err := http.NewRequest(http.MethodPost, upstream.URL, io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("request with non-replayable body must not be retried, hits = %d", hits.Load())
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		if !isRetryableStatus(code) {
			t.Fatalf("status %d should be retryable", code)
		}
	}
	for _, code := range []int{200, 301, 400, 401, 404} {
		if isRetryableStatus(code) {
			t.Fatalf("status %d should not be retryable", code)
		}
	}
}
