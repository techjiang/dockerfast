package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
	if cfg.def() == nil {
		t.Fatal("expected a default upstream")
	}
}

// def is a tiny test helper mirroring proxy's default lookup.
func (c *Config) def() *Upstream {
	for i := range c.Upstreams {
		if c.Upstreams[i].Default {
			return &c.Upstreams[i]
		}
	}
	return nil
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	content := `{"listen":":6000","cache":{"maxSizeBytes":1024}}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":6000" {
		t.Fatalf("listen = %q, want :6000", cfg.Listen)
	}
	if cfg.Cache.MaxSizeBytes != 1024 {
		t.Fatalf("maxSizeBytes = %d, want 1024", cfg.Cache.MaxSizeBytes)
	}
	// Fields absent from JSON must retain defaults.
	if cfg.Cache.BlobTTL.Duration() != 720*time.Hour {
		t.Fatalf("blobTTL default not preserved: %v", cfg.Cache.BlobTTL.Duration())
	}
	if len(cfg.Upstreams) == 0 {
		t.Fatal("expected default upstreams preserved")
	}
}

func TestDurationParsing(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90m"`)); err != nil {
		t.Fatal(err)
	}
	if d.Duration() != 90*time.Minute {
		t.Fatalf("got %v", d.Duration())
	}
	if err := d.UnmarshalJSON([]byte(`120`)); err != nil {
		t.Fatal(err)
	}
	if d.Duration() != 120*time.Second {
		t.Fatalf("numeric seconds: got %v", d.Duration())
	}
	if err := d.UnmarshalJSON([]byte(`"not-a-duration"`)); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("DOCKERFAST_LISTEN", ":7777")
	t.Setenv("DOCKERFAST_MAX_CACHE_BYTES", "2048")
	t.Setenv("DOCKERFAST_BLOB_TTL", "1h")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":7777" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.Cache.MaxSizeBytes != 2048 {
		t.Fatalf("maxSizeBytes = %d", cfg.Cache.MaxSizeBytes)
	}
	if cfg.Cache.BlobTTL.Duration() != time.Hour {
		t.Fatalf("blobTTL = %v", cfg.Cache.BlobTTL.Duration())
	}
}

func TestValidateRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p, []byte(`{"nope":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestValidateDuplicateUpstream(t *testing.T) {
	cfg := Default()
	cfg.Upstreams = append(cfg.Upstreams, cfg.Upstreams[0])
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate upstream error")
	}
}

func TestAuthValidation(t *testing.T) {
	cfg := Default()
	cfg.Auth = Auth{Enabled: true, Username: "admin"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when password missing")
	}
	cfg.Auth.Password = "secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("auth config should be valid: %v", err)
	}
}

func TestRetryDefaultsAndNormalisation(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Retry.Attempts != 3 {
		t.Fatalf("default attempts = %d, want 3", cfg.Retry.Attempts)
	}
	// Zero attempts must be coerced to 1.
	cfg2 := Default()
	cfg2.Retry.Attempts = 0
	if err := cfg2.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg2.Retry.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", cfg2.Retry.Attempts)
	}
}
