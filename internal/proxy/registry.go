// Package proxy implements a pull-through caching proxy for the Docker
// Registry V2 protocol. It supports multiple upstream registries, transparent
// token authentication, and a local content-addressed cache.
package proxy

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dockerfast/dockerfast/internal/cache"
	"github.com/dockerfast/dockerfast/internal/config"
)

// Route kinds recognised by the router.
const (
	kindBase      = "base"
	kindCatalog   = "catalog"
	kindManifest  = "manifest"
	kindBlob      = "blob"
	kindUpload    = "upload"
	kindTags      = "tags"
	kindReferrers = "referrers"
)

// defaultAccept is sent upstream when the client does not specify an Accept.
const defaultAccept = "application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v1+json"

// Registry proxies Registry V2 requests to configured upstreams.
type Registry struct {
	cfg     *config.Config
	cache   *cache.Cache
	log     *slog.Logger
	metrics *Metrics
	client  *http.Client

	upstreams []*config.Upstream
	byHost    map[string]*config.Upstream
	def       *config.Upstream

	version string

	flights flightGroup
}

// New builds a Registry from configuration and a cache.
func New(cfg *config.Config, c *cache.Cache, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{
		cfg:     cfg,
		cache:   c,
		log:     log,
		metrics: NewMetrics(),
		byHost:  make(map[string]*config.Upstream),
		flights: flightGroup{},
		version: "dev",
	}

	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]
		r.upstreams = append(r.upstreams, u)
		for _, h := range u.Hosts {
			r.byHost[strings.ToLower(h)] = u
		}
		if u.Default {
			r.def = u
		}
	}
	if r.def == nil {
		return nil, errors.New("proxy: no default upstream configured")
	}

	creds := make(map[string]credentials)
	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]
		if u.Username != "" || u.Password != "" {
			creds[hostOf(u.Endpoint)] = credentials{Username: u.Username, Password: u.Password, HasAuth: true}
		}
	}

	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	retrying := newRetryTransport(base, cfg.Retry.Attempts, cfg.Retry.InitialBackoff.Duration(), cfg.Retry.MaxBackoff.Duration())
	retrying.onRetry = r.metrics.IncRetries

	tr := newAuthTransport(retrying, creds)
	r.client = &http.Client{
		Transport: tr,
		Timeout:   cfg.UpstreamTimeout.Duration(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	return r, nil
}

// SetVersion records the build version for reporting in /status.
func (r *Registry) SetVersion(v string) {
	if v != "" {
		r.version = v
	}
}

// Metrics exposes the registry metrics collector.
func (r *Registry) Metrics() *Metrics { return r.metrics }

// Handler returns the HTTP handler serving the registry API plus operational
// endpoints (/healthz, /metrics, /status, /).
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", r.handleHealth)
	mux.HandleFunc("/metrics", r.handleMetrics)
	mux.HandleFunc("/status", r.handleStatus)
	mux.HandleFunc("/v2/", r.handleV2)
	mux.HandleFunc("/v2", r.handleV2)
	mux.HandleFunc("/", r.handleRoot)
	return r.withLogging(r.withAuth(mux))
}

func (r *Registry) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (r *Registry) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	r.metrics.WritePrometheus(w, r.cache)
}

// statusResponse is the machine-readable service status.
type statusResponse struct {
	Version       string           `json:"version"`
	UptimeSeconds int64            `json:"uptimeSeconds"`
	Cache         statusCache      `json:"cache"`
	Upstreams     []statusUpstream `json:"upstreams"`
	Metrics       map[string]int64 `json:"metrics"`
}

type statusCache struct {
	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"maxBytes"`
	Files    int   `json:"files"`
}

type statusUpstream struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Default  bool   `json:"default"`
}

func (r *Registry) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := statusResponse{
		Version:       r.version,
		UptimeSeconds: r.metrics.UptimeSeconds(),
		Cache: statusCache{
			Bytes:    r.cache.TotalBytes(),
			MaxBytes: r.cache.MaxBytes(),
			Files:    r.cache.FileCount(),
		},
		Metrics: r.metrics.Snapshot(),
	}
	for _, u := range r.upstreams {
		resp.Upstreams = append(resp.Upstreams, statusUpstream{Name: u.Name, Endpoint: u.Endpoint, Default: u.Default})
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// handleRoot serves a small self-contained status dashboard.
func (r *Registry) handleRoot(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, dashboardHTML)
}

func (r *Registry) handleV2(w http.ResponseWriter, req *http.Request) {
	r.metrics.IncRequests()
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	path := req.URL.Path
	if path == "/v2" || path == "/v2/" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
		return
	}

	rt, err := parseRoute(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", err.Error())
		return
	}
	up, repo := r.resolveUpstream(rt, req.Host)
	if up == nil {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "no upstream for request")
		return
	}

	switch rt.Kind {
	case kindManifest:
		r.handleManifest(w, req, rt, up, repo)
	case kindBlob:
		r.handleBlob(w, req, rt, up, repo)
	case kindTags:
		r.passthrough(w, req, up, repo, "/tags/list")
	case kindReferrers:
		r.passthrough(w, req, up, repo, "/referrers/"+rt.Ref)
	case kindCatalog:
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "catalog is not proxied")
	case kindUpload:
		if !r.cfg.AllowPush {
			writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "push is disabled")
			return
		}
		r.handleUpload(w, req, rt, up, repo)
	default:
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "unsupported route")
	}
}

// Route is a parsed registry request path.
type Route struct {
	Name string // repository name, possibly prefixed with an upstream host
	Kind string
	Ref  string // tag, digest or upload UUID
}

// parseRoute splits a /v2/... path into repository name, kind and reference.
func parseRoute(p string) (*Route, error) {
	rest := strings.TrimPrefix(p, "/v2/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return &Route{Kind: kindBase}, nil
	}
	if rest == "_catalog" {
		return &Route{Kind: kindCatalog}, nil
	}
	if strings.HasSuffix(rest, "/tags/list") {
		name := strings.TrimSuffix(rest, "/tags/list")
		if name == "" {
			return nil, errors.New("missing repository name")
		}
		return &Route{Name: name, Kind: kindTags}, nil
	}
	if i := strings.LastIndex(rest, "/manifests/"); i > 0 {
		return &Route{Name: rest[:i], Kind: kindManifest, Ref: rest[i+len("/manifests/"):]}, nil
	}
	if i := strings.LastIndex(rest, "/blobs/uploads"); i > 0 {
		uuid := strings.Trim(rest[i+len("/blobs/uploads"):], "/")
		return &Route{Name: rest[:i], Kind: kindUpload, Ref: uuid}, nil
	}
	if i := strings.LastIndex(rest, "/blobs/"); i > 0 {
		return &Route{Name: rest[:i], Kind: kindBlob, Ref: rest[i+len("/blobs/"):]}, nil
	}
	if i := strings.LastIndex(rest, "/referrers/"); i > 0 {
		return &Route{Name: rest[:i], Kind: kindReferrers, Ref: rest[i+len("/referrers/"):]}, nil
	}
	return nil, fmt.Errorf("unsupported registry path %q", p)
}

// resolveUpstream determines which upstream serves a route and returns the
// repository name relative to that upstream. Resolution order:
//  1. first path segment matching a configured upstream host alias
//  2. the request Host header matching an upstream host alias
//  3. the default upstream (Docker Hub)
func (r *Registry) resolveUpstream(rt *Route, reqHost string) (*config.Upstream, string) {
	repo := strings.Trim(rt.Name, "/")
	segs := strings.Split(repo, "/")
	if len(segs) >= 2 {
		if u, ok := r.byHost[strings.ToLower(segs[0])]; ok {
			return u, strings.Join(segs[1:], "/")
		}
	}
	if host := stripPort(reqHost); host != "" {
		if u, ok := r.byHost[strings.ToLower(host)]; ok {
			return u, repo
		}
	}
	if isDockerHub(r.def) && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return r.def, repo
}

// --- passthrough for unsupported-but-forwardable endpoints ---

var hopHeaders = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func (r *Registry) passthrough(w http.ResponseWriter, req *http.Request, up *config.Upstream, repo, suffix string) {
	r.metrics.IncBypass()
	setCacheStatus(w, cacheBypass)
	target := up.Endpoint + "/v2/" + repo + suffix
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	preq, err := http.NewRequestWithContext(req.Context(), req.Method, target, req.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "UNKNOWN", err.Error())
		return
	}
	copyHeaders(preq.Header, req.Header)
	resp, err := r.client.Do(preq)
	if err != nil {
		r.metrics.IncErrors()
		writeError(w, http.StatusBadGateway, "UNKNOWN", err.Error())
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if req.Method != http.MethodHead {
		n, _ := io.Copy(w, resp.Body)
		r.metrics.AddServed(n)
	}
}

func (r *Registry) handleUpload(w http.ResponseWriter, req *http.Request, rt *Route, up *config.Upstream, repo string) {
	suffix := "/blobs/uploads/"
	if rt.Ref != "" {
		suffix += rt.Ref
	}
	r.passthrough(w, req, up, repo, suffix)
}

// --- helpers ---

// registryError is the standard Registry V2 error envelope.
type registryError struct {
	Errors []registryErrorItem `json:"errors"`
}

// upstreamError carries the HTTP status returned by an upstream registry so the
// caller can translate it into an appropriate client response.
type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

func statusFromError(err error) int {
	if ue, ok := err.(*upstreamError); ok {
		switch ue.status {
		case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
			return ue.status
		}
	}
	return http.StatusBadGateway
}

type registryErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(registryError{Errors: []registryErrorItem{{Code: code, Message: message}}})
}

// cacheStatusHeader reports whether a response was served from cache.
const cacheStatusHeader = "X-DockerFast-Cache"

// Cache status values.
const (
	cacheHit    = "HIT"
	cacheMiss   = "MISS"
	cacheBypass = "BYPASS"
)

func setCacheStatus(w http.ResponseWriter, status string) {
	w.Header().Set(cacheStatusHeader, status)
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

func stripPort(host string) string {
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func isDockerHub(u *config.Upstream) bool {
	if u == nil {
		return false
	}
	if u.Name == "docker.io" {
		return true
	}
	for _, h := range u.Hosts {
		if h == "registry-1.docker.io" || h == "index.docker.io" {
			return true
		}
	}
	return false
}

// validRef guards against path traversal / injection in tag and digest refs.
func validRef(ref string) bool {
	if ref == "" || len(ref) > 256 {
		return false
	}
	for _, c := range ref {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_', c == ':', c == '+':
		default:
			return false
		}
	}
	return true
}

// --- single-flight for identical concurrent downloads ---

type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightEntry
}

type flightEntry struct {
	mu   sync.Mutex
	refs int
}

// Do runs fn exclusively for the given key. Concurrent calls with the same key
// are serialised; the caller is expected to re-check the cache inside fn.
func (g *flightGroup) Do(key string, fn func() error) error {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightEntry)
	}
	e := g.m[key]
	if e == nil {
		e = &flightEntry{}
		g.m[key] = e
	}
	e.refs++
	g.mu.Unlock()

	e.mu.Lock()
	err := fn()
	e.mu.Unlock()

	g.mu.Lock()
	e.refs--
	if e.refs == 0 {
		delete(g.m, key)
	}
	g.mu.Unlock()
	return err
}
