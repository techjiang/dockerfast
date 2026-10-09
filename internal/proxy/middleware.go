package proxy

import (
	"net/http"
	"time"
)

// statusRecorder captures the response status code and byte count for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher so long streaming responses are not buffered.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withLogging wraps a handler with access logging and request accounting.
func (r *Registry) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, req)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		if rec.bytes > 0 {
			r.metrics.AddServed(rec.bytes)
		}
		if r.cfg.AccessLog {
			r.log.Info("request",
				"method", req.Method,
				"path", req.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration", time.Since(start).Round(time.Millisecond).String(),
				"remote", req.RemoteAddr,
			)
		}
	})
}
