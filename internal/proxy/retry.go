package proxy

import (
	"fmt"
	"math/rand"
	"net/http"
	"time"
)

// retryTransport retries idempotent requests that fail with a transient network
// error or a retryable HTTP status, using exponential backoff with jitter.
type retryTransport struct {
	base     http.RoundTripper
	attempts int
	initial  time.Duration
	max      time.Duration
	onRetry  func()
	// rand is injectable for deterministic tests.
	rand func() float64
}

func newRetryTransport(base http.RoundTripper, attempts int, initial, max time.Duration) *retryTransport {
	if attempts < 1 {
		attempts = 1
	}
	if initial <= 0 {
		initial = 200 * time.Millisecond
	}
	if max <= 0 {
		max = 2 * time.Second
	}
	return &retryTransport{
		base:     base,
		attempts: attempts,
		initial:  initial,
		max:      max,
		rand:     rand.Float64,
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A request with a non-replayable body must not be retried.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return t.base.RoundTrip(req)
	}

	attempts := t.attempts
	backoff := t.initial
	for attempt := 1; ; attempt++ {
		attemptReq := req.Clone(req.Context())
		if req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("retry: rebuild body: %w", err)
			}
			attemptReq.Body = b
		}
		resp, err := t.base.RoundTrip(attemptReq)
		if err != nil {
			if attempt >= attempts {
				return nil, err
			}
		} else if isRetryableStatus(resp.StatusCode) && attempt < attempts {
			drainClose(resp.Body)
		} else {
			return resp, nil
		}

		if t.onRetry != nil {
			t.onRetry()
		}
		if err := sleepBackoff(req, backoff, t.rand); err != nil {
			return nil, err
		}
		backoff *= 2
		if backoff > t.max {
			backoff = t.max
		}
	}
}

// sleepBackoff waits for the backoff duration (with up to 50% jitter) or until
// the request context is cancelled.
func sleepBackoff(req *http.Request, backoff time.Duration, rnd func() float64) error {
	if backoff <= 0 {
		return nil
	}
	// Add up to 50% jitter.
	d := backoff + time.Duration(float64(backoff)*0.5*rnd())
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-req.Context().Done():
		return req.Context().Err()
	case <-timer.C:
		return nil
	}
}
