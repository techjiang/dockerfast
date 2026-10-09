package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/dockerfast/dockerfast/internal/cache"
	"github.com/dockerfast/dockerfast/internal/config"
)

// handleBlob serves blob (image layer / config) requests.
func (r *Registry) handleBlob(w http.ResponseWriter, req *http.Request, rt *Route, up *config.Upstream, repo string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	d, err := cache.ParseDigest(rt.Ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")
		return
	}

	// Fast path: serve directly from cache (supports range requests).
	if f, _, oerr := r.cache.OpenBlob(d); oerr == nil {
		r.metrics.IncBlobHit()
		serveBlobFile(w, req, d, f)
		return
	}

	// Not cached. Range or HEAD requests are proxied straight to the upstream
	// without caching the (partial) content.
	if req.Method == http.MethodHead || req.Header.Get("Range") != "" {
		r.passthrough(w, req, up, repo, "/blobs/"+d.String())
		return
	}

	// Concurrent identical downloads are coalesced: the winner streams to its
	// client while filling the cache; losers wait, then serve from cache.
	streamed := false
	started := false
	var streamErr error
	key := up.Name + "/" + repo + "/" + d.String()
	_ = r.flights.Do(key, func() error {
		// Re-check after acquiring the flight slot.
		if r.cache.HasBlob(d) {
			return nil
		}
		streamed = true
		started, streamErr = r.streamAndCache(req, w, up, repo, d)
		return streamErr
	})

	if streamed {
		if streamErr != nil {
			r.metrics.IncErrors()
			if started {
				// Response already partially written; only log.
				r.log.Warn("blob stream failed", "error", streamErr, "digest", d.String(), "repo", repo)
			} else {
				writeError(w, statusFromError(streamErr), "BLOB_UNKNOWN", streamErr.Error())
			}
		}
		return
	}

	// We waited on another download that populated the cache.
	f, _, err := r.cache.OpenBlob(d)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BLOB_UNKNOWN", "blob unavailable after download")
		return
	}
	r.metrics.IncBlobHit()
	serveBlobFile(w, req, d, f)
}

// streamAndCache fetches a blob from upstream, streams it to the client and
// writes it to the cache at the same time. The content digest is verified.
//
// It returns started=true once the response headers have been written to the
// client; after that point an error can no longer be reported to the client.
func (r *Registry) streamAndCache(req *http.Request, w http.ResponseWriter, up *config.Upstream, repo string, d cache.Digest) (started bool, err error) {
	target := up.Endpoint + "/v2/" + repo + "/blobs/" + d.String()
	preq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	preq.Header.Set("User-Agent", "dockerfast")

	resp, err := r.client.Do(preq)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &upstreamError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("upstream %s returned %s for blob %s", up.Name, resp.Status, d.String()),
		}
	}

	tw, err := r.cache.NewTempWriter()
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			tw.Abort()
		}
	}()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Accept-Ranges", "bytes")
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	started = true

	h := sha256.New()
	mw := io.MultiWriter(w, tw, h)
	n, err := io.Copy(mw, resp.Body)
	if err != nil {
		return started, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != d.Hex {
		return started, fmt.Errorf("digest mismatch for %s: got sha256:%s", d.String(), sum)
	}
	if err := tw.Commit(d); err != nil {
		return started, err
	}
	committed = true
	r.metrics.IncBlobMiss()
	r.metrics.AddFetched(n)
	return started, nil
}

// serveBlobFile writes a cached blob file, honouring range requests.
func serveBlobFile(w http.ResponseWriter, req *http.Request, d cache.Digest, f *os.File) {
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d.String())
	http.ServeContent(w, req, "", time.Time{}, f)
}
