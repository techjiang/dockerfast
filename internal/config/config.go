// Package config defines the DockerFast runtime configuration, its defaults,
// JSON loading and environment variable overrides.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration is a JSON-friendly duration. It accepts either a Go duration string
// such as "30m" or "720h", or a plain number which is interpreted as seconds.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		if str == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(str)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", str, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("invalid duration %s", s)
	}
	*d = Duration(time.Duration(n) * time.Second)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Upstream describes a single remote registry that DockerFast can proxy.
type Upstream struct {
	// Name is a stable identifier used as a cache namespace (e.g. "docker.io").
	Name string `json:"name"`
	// Hosts are aliases that may appear as the first path segment in a request
	// (for example "ghcr.io/owner/repo") to route to this upstream.
	Hosts    []string `json:"hosts"`
	Endpoint string   `json:"endpoint"`
	Default  bool     `json:"default"`
	// Username/Password are optional credentials used to obtain a pull token or
	// to authenticate with a private registry.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// InsecureSkipVerify disables TLS certificate verification for this upstream.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// Cache configures the on-disk cache behaviour.
type Cache struct {
	MaxSizeBytes    int64    `json:"maxSizeBytes"`
	BlobTTL         Duration `json:"blobTTL"`
	ManifestTTL     Duration `json:"manifestTTL"`
	CleanupInterval Duration `json:"cleanupInterval"`
}

// Retry configures retry behaviour for transient upstream failures.
type Retry struct {
	// Attempts is the total number of attempts (1 disables retrying).
	Attempts       int      `json:"attempts"`
	InitialBackoff Duration `json:"initialBackoff"`
	MaxBackoff     Duration `json:"maxBackoff"`
}

// Auth optionally protects the proxy with HTTP Basic authentication. This is
// useful when the cache is exposed to an untrusted network.
type Auth struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Config is the top-level configuration.
type Config struct {
	Listen          string     `json:"listen"`
	DataDir         string     `json:"dataDir"`
	LogLevel        string     `json:"logLevel"`
	AccessLog       bool       `json:"accessLog"`
	AllowPush       bool       `json:"allowPush"`
	MaxManifestSize int64      `json:"maxManifestSize"`
	UpstreamTimeout Duration   `json:"upstreamTimeout"`
	Cache           Cache      `json:"cache"`
	Retry           Retry      `json:"retry"`
	Auth            Auth       `json:"auth"`
	Upstreams       []Upstream `json:"upstreams"`
}

// Default returns a configuration populated with sensible defaults.
func Default() *Config {
	return &Config{
		Listen:          ":5000",
		DataDir:         "./data",
		LogLevel:        "info",
		AccessLog:       true,
		AllowPush:       false,
		MaxManifestSize: 8 << 20, // 8 MiB
		UpstreamTimeout: Duration(5 * time.Minute),
		Cache: Cache{
			MaxSizeBytes:    20 << 30, // 20 GiB
			BlobTTL:         Duration(720 * time.Hour),
			ManifestTTL:     Duration(24 * time.Hour),
			CleanupInterval: Duration(30 * time.Minute),
		},
		Retry: Retry{
			Attempts:       3,
			InitialBackoff: Duration(200 * time.Millisecond),
			MaxBackoff:     Duration(2 * time.Second),
		},
		Upstreams: DefaultUpstreams(),
	}
}

// DefaultUpstreams returns the built-in registry list. Docker Hub is the
// default upstream used when a request does not name another registry.
func DefaultUpstreams() []Upstream {
	return []Upstream{
		{Name: "docker.io", Hosts: []string{"docker.io", "registry-1.docker.io", "index.docker.io"}, Endpoint: "https://registry-1.docker.io", Default: true},
		{Name: "ghcr.io", Hosts: []string{"ghcr.io"}, Endpoint: "https://ghcr.io"},
		{Name: "gcr.io", Hosts: []string{"gcr.io"}, Endpoint: "https://gcr.io"},
		{Name: "k8s.gcr.io", Hosts: []string{"k8s.gcr.io"}, Endpoint: "https://k8s.gcr.io"},
		{Name: "registry.k8s.io", Hosts: []string{"registry.k8s.io"}, Endpoint: "https://registry.k8s.io"},
		{Name: "quay.io", Hosts: []string{"quay.io"}, Endpoint: "https://quay.io"},
		{Name: "mcr.microsoft.com", Hosts: []string{"mcr.microsoft.com"}, Endpoint: "https://mcr.microsoft.com"},
		{Name: "public.ecr.aws", Hosts: []string{"public.ecr.aws"}, Endpoint: "https://public.ecr.aws"},
		{Name: "nvcr.io", Hosts: []string{"nvcr.io"}, Endpoint: "https://nvcr.io"},
		{Name: "registry.gitlab.com", Hosts: []string{"registry.gitlab.com"}, Endpoint: "https://registry.gitlab.com"},
		{Name: "docker.elastic.co", Hosts: []string{"docker.elastic.co"}, Endpoint: "https://docker.elastic.co"},
	}
}

// Load reads a JSON config file (path may be empty to use defaults only),
// applies environment overrides and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("DOCKERFAST_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("DOCKERFAST_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("DOCKERFAST_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v, ok := envBool("DOCKERFAST_ACCESS_LOG"); ok {
		c.AccessLog = v
	}
	if v, ok := envBool("DOCKERFAST_ALLOW_PUSH"); ok {
		c.AllowPush = v
	}
	if v, ok := envInt("DOCKERFAST_MAX_CACHE_BYTES"); ok {
		c.Cache.MaxSizeBytes = v
	}
	if v, ok := envDuration("DOCKERFAST_BLOB_TTL"); ok {
		c.Cache.BlobTTL = Duration(v)
	}
	if v, ok := envDuration("DOCKERFAST_MANIFEST_TTL"); ok {
		c.Cache.ManifestTTL = Duration(v)
	}
	if v, ok := envInt("DOCKERFAST_RETRY_ATTEMPTS"); ok {
		c.Retry.Attempts = int(v)
	}
	if v, ok := envBool("DOCKERFAST_AUTH_ENABLED"); ok {
		c.Auth.Enabled = v
	}
	if v := os.Getenv("DOCKERFAST_AUTH_USERNAME"); v != "" {
		c.Auth.Username = v
	}
	if v := os.Getenv("DOCKERFAST_AUTH_PASSWORD"); v != "" {
		c.Auth.Password = v
	}
}

// Validate checks the configuration for obvious mistakes.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("config: listen address must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: dataDir must not be empty")
	}
	if c.MaxManifestSize <= 0 {
		return fmt.Errorf("config: maxManifestSize must be positive")
	}
	if c.Retry.Attempts < 1 {
		c.Retry.Attempts = 1
	}
	if c.Retry.InitialBackoff.Duration() <= 0 {
		c.Retry.InitialBackoff = Duration(200 * time.Millisecond)
	}
	if c.Retry.MaxBackoff.Duration() < c.Retry.InitialBackoff.Duration() {
		c.Retry.MaxBackoff = c.Retry.InitialBackoff
	}
	if c.Auth.Enabled {
		if c.Auth.Username == "" {
			return fmt.Errorf("config: auth.username must be set when auth is enabled")
		}
		if c.Auth.Password == "" {
			return fmt.Errorf("config: auth.password must be set when auth is enabled")
		}
	}
	defaults := 0
	seen := map[string]bool{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Name == "" {
			return fmt.Errorf("config: upstream #%d has empty name", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("config: duplicate upstream name %q", u.Name)
		}
		seen[u.Name] = true
		if u.Endpoint == "" {
			u.Endpoint = "https://" + u.Name
		}
		pu, err := url.Parse(u.Endpoint)
		if err != nil || pu.Scheme == "" || pu.Host == "" {
			return fmt.Errorf("config: upstream %q has invalid endpoint %q", u.Name, u.Endpoint)
		}
		u.Endpoint = strings.TrimRight(u.Endpoint, "/")
		if len(u.Hosts) == 0 {
			u.Hosts = []string{u.Name}
		}
		if u.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("config: only one upstream may be marked default")
	}
	if defaults == 0 {
		c.Upstreams[0].Default = true
	}
	return nil
}

func envBool(key string) (bool, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

func envInt(key string) (int64, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func envDuration(key string) (time.Duration, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false
	}
	return d, true
}
