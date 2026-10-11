package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dockerfast/dockerfast/internal/cache"
	"github.com/dockerfast/dockerfast/internal/config"
)

// handleManifest serves manifest requests from cache or by fetching upstream.
func (r *Registry) handleManifest(w http.ResponseWriter, req *http.Request, rt *Route, up *config.Upstream, repo string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		if r.cfg.AllowPush && (req.Method == http.MethodPut || req.Method == http.MethodDelete) {
			r.passthrough(w, req, up, repo, "/manifests/"+rt.Ref)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	if !validRef(rt.Ref) {
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid reference")
		return
	}

	refIsDigest := strings.Contains(rt.Ref, ":")
	var digestStr string
	if refIsDigest {
		digestStr = rt.Ref
	} else if d, ok := r.cache.GetRef(up.Name, repo, rt.Ref); ok {
		digestStr = d
	}

	// Fast path: content-addressed manifest is cached.
	if digestStr != "" {
		if d, err := cache.ParseDigest(digestStr); err == nil {
			if data, media, ok := r.cache.GetManifest(d); ok {
				r.metrics.IncManifestHit()
				setCacheStatus(w, cacheHit)
				writeManifest(w, req, d.String(), media, data)
				return
			}
		}
	}

	// Miss: fetch from upstream, then cache and serve.
	data, media, digest, err := r.fetchManifest(req, up, repo, rt.Ref, req.Header.Get("Accept"))
	if err != nil {
		r.metrics.IncErrors()
		writeError(w, statusFromError(err), "MANIFEST_UNKNOWN", err.Error())
		return
	}
	r.metrics.IncManifestMiss()
	setCacheStatus(w, cacheMiss)

	if d, perr := cache.ParseDigest(digest); perr == nil {
		if err := r.cache.PutManifest(d, media, data); err != nil {
			r.log.Warn("cache manifest failed", "error", err, "digest", digest)
		}
		if !refIsDigest {
			if err := r.cache.PutRef(up.Name, repo, rt.Ref, d.String()); err != nil {
				r.log.Warn("cache ref failed", "error", err, "ref", rt.Ref)
			}
		}
	}
	writeManifest(w, req, digest, media, data)
}

// fetchManifest downloads a manifest from the upstream registry.
func (r *Registry) fetchManifest(req *http.Request, up *config.Upstream, repo, ref, accept string) ([]byte, string, string, error) {
	target := up.Endpoint + "/v2/" + repo + "/manifests/" + ref
	preq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, "", "", err
	}
	// Preserve the client's Accept negotiation for multi-arch manifests.
	if accept == "" {
		accept = defaultAccept
	}
	preq.Header.Set("Accept", accept)
	preq.Header.Set("User-Agent", "dockerfast")

	resp, err := r.client.Do(preq)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", &upstreamError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("upstream %s returned %s for %s/%s", up.Name, resp.Status, repo, ref),
		}
	}

	limited := io.LimitReader(resp.Body, r.cfg.MaxManifestSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(data)) > r.cfg.MaxManifestSize {
		return nil, "", "", fmt.Errorf("manifest exceeds max size %d", r.cfg.MaxManifestSize)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(data)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	media := resp.Header.Get("Content-Type")
	if media == "" {
		media = "application/vnd.docker.distribution.manifest.v2+json"
	}
	return data, media, digest, nil
}

// writeManifest writes a manifest response with the correct registry headers.
func writeManifest(w http.ResponseWriter, req *http.Request, digest, media string, data []byte) {
	if media == "" {
		media = "application/vnd.docker.distribution.manifest.v2+json"
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if req.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
