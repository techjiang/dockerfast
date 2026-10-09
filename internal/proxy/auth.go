package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// credentials holds optional upstream authentication details.
type credentials struct {
	Username string
	Password string
	HasAuth  bool
}

type tokenKey struct{ realm, service, scope string }

type cachedToken struct {
	token   string
	expires time.Time
}

// authTransport is an http.RoundTripper that transparently handles the registry
// token (Bearer) authentication handshake. When an upstream replies 401 with a
// WWW-Authenticate challenge, it fetches a pull token and retries once. Tokens
// are cached until shortly before they expire.
type authTransport struct {
	base  http.RoundTripper
	plain http.RoundTripper // used for token endpoint calls (no auth recursion)
	creds map[string]credentials
	now   func() time.Time

	mu     sync.Mutex
	tokens map[tokenKey]cachedToken
}

func newAuthTransport(base http.RoundTripper, creds map[string]credentials) *authTransport {
	return &authTransport{
		base:   base,
		plain:  base,
		creds:  creds,
		now:    time.Now,
		tokens: make(map[tokenKey]cachedToken),
	}
}

func (t *authTransport) credsFor(host string) credentials {
	if c, ok := t.creds[host]; ok {
		return c
	}
	return credentials{}
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if challenge == "" {
		return resp, nil
	}
	scheme, params := parseChallenge(challenge)
	creds := t.credsFor(req.URL.Host)

	var authz string
	switch strings.ToLower(scheme) {
	case "bearer":
		scope := params["scope"]
		if scope == "" {
			scope = scopeFromPath(req.URL.Path)
		}
		tok, terr := t.token(req.Context(), params, scope, creds)
		if terr != nil {
			// Surface the original 401 rather than a transport error.
			drainClose(resp.Body)
			return &http.Response{
				Status:     resp.Status,
				StatusCode: resp.StatusCode,
				Proto:      resp.Proto,
				ProtoMajor: resp.ProtoMajor,
				ProtoMinor: resp.ProtoMinor,
				Header:     resp.Header,
				Request:    req,
				Body:       http.NoBody,
			}, nil
		}
		authz = "Bearer " + tok

	case "basic":
		if !creds.HasAuth {
			return resp, nil
		}
		authz = "Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password))

	default:
		return resp, nil
	}

	drainClose(resp.Body)
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, berr := req.GetBody()
		if berr == nil {
			retry.Body = body
		}
	}
	retry.Header.Set("Authorization", authz)
	return t.base.RoundTrip(retry)
}

// token obtains (and caches) a bearer token for the given challenge.
func (t *authTransport) token(ctx context.Context, params map[string]string, scope string, creds credentials) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("auth: missing realm in challenge")
	}
	service := params["service"]
	key := tokenKey{realm: realm, service: service, scope: scope}

	t.mu.Lock()
	if ct, ok := t.tokens[key]; ok && t.now().Before(ct.expires) {
		t.mu.Unlock()
		return ct.token, nil
	}
	t.mu.Unlock()

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("auth: bad realm %q: %w", realm, err)
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if creds.HasAuth {
		treq.SetBasicAuth(creds.Username, creds.Password)
	}
	treq.Header.Set("Accept", "application/json")

	resp, err := t.plain.RoundTrip(treq)
	if err != nil {
		return "", fmt.Errorf("auth: token request: %w", err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: token endpoint returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("auth: decode token: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("auth: empty token in response")
	}
	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 60
	}
	if ttl > 10 {
		ttl -= 10 // refresh a little early
	}
	t.mu.Lock()
	t.tokens[key] = cachedToken{token: tok, expires: t.now().Add(time.Duration(ttl) * time.Second)}
	t.mu.Unlock()
	return tok, nil
}

// parseChallenge parses a WWW-Authenticate header such as:
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"
func parseChallenge(h string) (scheme string, params map[string]string) {
	params = map[string]string{}
	h = strings.TrimSpace(h)
	sp := strings.IndexByte(h, ' ')
	if sp < 0 {
		return h, params
	}
	scheme = h[:sp]
	rest := h[sp+1:]
	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, "\"") {
			rest = rest[1:]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				val = rest
				rest = ""
			} else {
				val = rest[:end]
				rest = rest[end+1:]
			}
		} else {
			comma := strings.IndexByte(rest, ',')
			if comma < 0 {
				val = rest
				rest = ""
			} else {
				val = rest[:comma]
				rest = rest[comma:]
			}
		}
		params[strings.ToLower(key)] = val
	}
	return scheme, params
}

// scopeFromPath derives a pull scope from a registry API path.
//
//	/v2/library/nginx/manifests/latest -> repository:library/nginx:pull
func scopeFromPath(p string) string {
	rest := strings.TrimPrefix(p, "/v2/")
	for _, marker := range []string{"/manifests/", "/blobs/", "/tags/", "/referrers/"} {
		if i := strings.Index(rest, marker); i >= 0 {
			rest = rest[:i]
			break
		}
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return ""
	}
	return "repository:" + rest + ":pull"
}

func drainClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<16))
	_ = rc.Close()
}
