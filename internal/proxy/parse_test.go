package proxy

import (
	"testing"

	"github.com/dockerfast/dockerfast/internal/config"
)

func TestParseRoute(t *testing.T) {
	cases := []struct {
		path string
		kind string
		name string
		ref  string
	}{
		{"/v2/", kindBase, "", ""},
		{"/v2/library/nginx/manifests/latest", kindManifest, "library/nginx", "latest"},
		{"/v2/foo/bar/baz/manifests/sha256:abc", kindManifest, "foo/bar/baz", "sha256:abc"},
		{"/v2/library/nginx/blobs/sha256:deadbeef", kindBlob, "library/nginx", "sha256:deadbeef"},
		{"/v2/ghcr.io/owner/repo/blobs/sha256:aa", kindBlob, "ghcr.io/owner/repo", "sha256:aa"},
		{"/v2/library/nginx/tags/list", kindTags, "library/nginx", ""},
		{"/v2/library/nginx/blobs/uploads/", kindUpload, "library/nginx", ""},
		{"/v2/library/nginx/blobs/uploads/uuid-123", kindUpload, "library/nginx", "uuid-123"},
		{"/v2/library/nginx/referrers/sha256:abc", kindReferrers, "library/nginx", "sha256:abc"},
		{"/v2/_catalog", kindCatalog, "", ""},
	}
	for _, tc := range cases {
		rt, err := parseRoute(tc.path)
		if err != nil {
			t.Fatalf("parseRoute(%q) error: %v", tc.path, err)
		}
		if rt.Kind != tc.kind || rt.Name != tc.name || rt.Ref != tc.ref {
			t.Fatalf("parseRoute(%q) = {%s %s %s}, want {%s %s %s}",
				tc.path, rt.Kind, rt.Name, rt.Ref, tc.kind, tc.name, tc.ref)
		}
	}
	if _, err := parseRoute("/v2/garbage"); err == nil {
		t.Fatal("expected error for unsupported path")
	}
}

func TestResolveUpstream(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &Registry{byHost: map[string]*config.Upstream{}}
	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]
		r.upstreams = append(r.upstreams, u)
		for _, h := range u.Hosts {
			r.byHost[h] = u
		}
		if u.Default {
			r.def = u
		}
	}

	// Docker Hub default; single-component repo gets library/ prefix.
	up, repo := r.resolveUpstream(&Route{Name: "nginx"}, "")
	if up.Name != "docker.io" || repo != "library/nginx" {
		t.Fatalf("default resolve = %s/%s", up.Name, repo)
	}
	// Explicit host prefix.
	up, repo = r.resolveUpstream(&Route{Name: "ghcr.io/owner/repo"}, "")
	if up.Name != "ghcr.io" || repo != "owner/repo" {
		t.Fatalf("prefix resolve = %s/%s", up.Name, repo)
	}
	// Host header hint.
	up, repo = r.resolveUpstream(&Route{Name: "owner/repo"}, "quay.io")
	if up.Name != "quay.io" || repo != "owner/repo" {
		t.Fatalf("host-hint resolve = %s/%s", up.Name, repo)
	}
	// Host header with port.
	up, repo = r.resolveUpstream(&Route{Name: "owner/repo"}, "quay.io:5000")
	if up.Name != "quay.io" || repo != "owner/repo" {
		t.Fatalf("host-hint-with-port resolve = %s/%s", up.Name, repo)
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if scheme != "Bearer" {
		t.Fatalf("scheme = %q", scheme)
	}
	if params["realm"] != "https://auth.docker.io/token" {
		t.Fatalf("realm = %q", params["realm"])
	}
	if params["service"] != "registry.docker.io" {
		t.Fatalf("service = %q", params["service"])
	}
	if params["scope"] != "repository:library/nginx:pull" {
		t.Fatalf("scope = %q", params["scope"])
	}

	scheme, _ = parseChallenge("Basic")
	if scheme != "Basic" {
		t.Fatalf("basic scheme = %q", scheme)
	}

	// Unquoted values.
	_, params = parseChallenge(`Bearer realm=https://r.io/token,service=svc`)
	if params["realm"] != "https://r.io/token" || params["service"] != "svc" {
		t.Fatalf("unquoted parse = %#v", params)
	}
}

func TestScopeFromPath(t *testing.T) {
	cases := map[string]string{
		"/v2/library/nginx/manifests/latest": "repository:library/nginx:pull",
		"/v2/foo/bar/blobs/sha256:aa":        "repository:foo/bar:pull",
		"/v2/":                               "",
	}
	for in, want := range cases {
		if got := scopeFromPath(in); got != want {
			t.Fatalf("scopeFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidRef(t *testing.T) {
	for _, ok := range []string{"latest", "v1.2.3", "sha256:" + "abc", "1.0-alpine_amd64", "a+b"} {
		if !validRef(ok) {
			t.Fatalf("validRef(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "a/b", "../etc", "a b", "a\\b"} {
		if validRef(bad) {
			t.Fatalf("validRef(%q) = true, want false", bad)
		}
	}
}
