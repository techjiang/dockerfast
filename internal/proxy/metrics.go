package proxy

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/dockerfast/dockerfast/internal/cache"
)

// Metrics holds atomic counters exposed in Prometheus text format.
type Metrics struct {
	start time.Time

	requests       atomic.Int64
	manifestHits   atomic.Int64
	manifestMisses atomic.Int64
	blobHits       atomic.Int64
	blobMisses     atomic.Int64
	bytesServed    atomic.Int64
	bytesFetched   atomic.Int64
	errors         atomic.Int64
	retries        atomic.Int64
	bypasses       atomic.Int64
	inFlight       atomic.Int64
}

// NewMetrics creates a metrics collector.
func NewMetrics() *Metrics { return &Metrics{start: time.Now()} }

func (m *Metrics) IncRequests()       { m.requests.Add(1) }
func (m *Metrics) IncManifestHit()    { m.manifestHits.Add(1) }
func (m *Metrics) IncManifestMiss()   { m.manifestMisses.Add(1) }
func (m *Metrics) IncBlobHit()        { m.blobHits.Add(1) }
func (m *Metrics) IncBlobMiss()       { m.blobMisses.Add(1) }
func (m *Metrics) IncErrors()         { m.errors.Add(1) }
func (m *Metrics) IncRetries()        { m.retries.Add(1) }
func (m *Metrics) IncBypass()         { m.bypasses.Add(1) }
func (m *Metrics) IncInFlight()       { m.inFlight.Add(1) }
func (m *Metrics) DecInFlight()       { m.inFlight.Add(-1) }
func (m *Metrics) AddServed(n int64)  { m.bytesServed.Add(n) }
func (m *Metrics) AddFetched(n int64) { m.bytesFetched.Add(n) }

// UptimeSeconds returns the seconds elapsed since the process started.
func (m *Metrics) UptimeSeconds() int64 { return int64(time.Since(m.start).Seconds()) }

// Snapshot returns all counters keyed by their Prometheus metric name.
func (m *Metrics) Snapshot() map[string]int64 {
	return map[string]int64{
		"dockerfast_requests_total":              m.requests.Load(),
		"dockerfast_manifest_cache_hits_total":   m.manifestHits.Load(),
		"dockerfast_manifest_cache_misses_total": m.manifestMisses.Load(),
		"dockerfast_blob_cache_hits_total":       m.blobHits.Load(),
		"dockerfast_blob_cache_misses_total":     m.blobMisses.Load(),
		"dockerfast_bytes_served_total":          m.bytesServed.Load(),
		"dockerfast_bytes_fetched_total":         m.bytesFetched.Load(),
		"dockerfast_errors_total":                m.errors.Load(),
		"dockerfast_retries_total":               m.retries.Load(),
		"dockerfast_cache_bypass_total":          m.bypasses.Load(),
		"dockerfast_in_flight_requests":          m.inFlight.Load(),
	}
}

// WritePrometheus writes the metrics snapshot in Prometheus exposition format.
func (m *Metrics) WritePrometheus(w http.ResponseWriter, c *cache.Cache) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	type metric struct {
		name  string
		help  string
		mtype string
		value int64
	}
	metrics := []metric{
		{"dockerfast_requests_total", "Total registry API requests handled.", "counter", m.requests.Load()},
		{"dockerfast_manifest_cache_hits_total", "Manifest requests served from cache.", "counter", m.manifestHits.Load()},
		{"dockerfast_manifest_cache_misses_total", "Manifest requests fetched from upstream.", "counter", m.manifestMisses.Load()},
		{"dockerfast_blob_cache_hits_total", "Blob requests served from cache.", "counter", m.blobHits.Load()},
		{"dockerfast_blob_cache_misses_total", "Blob requests fetched from upstream.", "counter", m.blobMisses.Load()},
		{"dockerfast_cache_bypass_total", "Requests forwarded without caching.", "counter", m.bypasses.Load()},
		{"dockerfast_retries_total", "Upstream request retries performed.", "counter", m.retries.Load()},
		{"dockerfast_bytes_served_total", "Bytes served to clients.", "counter", m.bytesServed.Load()},
		{"dockerfast_bytes_fetched_total", "Bytes fetched from upstreams.", "counter", m.bytesFetched.Load()},
		{"dockerfast_errors_total", "Total errors encountered.", "counter", m.errors.Load()},
		{"dockerfast_in_flight_requests", "Requests currently being processed.", "gauge", m.inFlight.Load()},
		{"dockerfast_cache_bytes", "Current cache size in bytes.", "gauge", c.TotalBytes()},
		{"dockerfast_cache_max_bytes", "Configured cache size limit in bytes.", "gauge", c.MaxBytes()},
		{"dockerfast_cache_files", "Number of files tracked in the cache.", "gauge", int64(c.FileCount())},
		{"dockerfast_uptime_seconds", "Seconds since the process started.", "gauge", m.UptimeSeconds()},
	}
	for _, mt := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n", mt.name, mt.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", mt.name, mt.mtype)
		fmt.Fprintf(w, "%s %d\n", mt.name, mt.value)
	}
}
