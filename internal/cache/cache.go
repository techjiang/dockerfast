// Package cache implements a content-addressed on-disk cache for registry
// blobs and manifests, plus small reference pointers that map a tag to its
// resolved manifest digest.
//
// Layout under the configured root directory:
//
//	blobs/<algo>/<hex>          raw blob content
//	manifests/<algo>/<hex>      manifest bytes
//	manifests/<algo>/<hex>.media  manifest media type (sidecar)
//	refs/<upstream>/<repo>/<ref>  text file containing the manifest digest
//	tmp/                        in-progress atomic writes
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sentinel errors returned by the cache.
var (
	ErrNotFound      = errors.New("cache: not found")
	ErrInvalidDigest = errors.New("cache: invalid digest")
)

// Digest is a parsed content digest such as "sha256:abcd...".
type Digest struct {
	Algorithm string
	Hex       string
}

// ParseDigest validates and parses a digest string.
func ParseDigest(s string) (Digest, error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return Digest{}, ErrInvalidDigest
	}
	algo, hexv := s[:i], s[i+1:]
	var wantLen int
	switch algo {
	case "sha256":
		wantLen = 64
	case "sha512":
		wantLen = 128
	default:
		return Digest{}, fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidDigest, algo)
	}
	if len(hexv) != wantLen {
		return Digest{}, fmt.Errorf("%w: expected %d hex chars, got %d", ErrInvalidDigest, wantLen, len(hexv))
	}
	if _, err := hex.DecodeString(hexv); err != nil {
		return Digest{}, fmt.Errorf("%w: %v", ErrInvalidDigest, err)
	}
	return Digest{Algorithm: algo, Hex: hexv}, nil
}

// String returns the canonical "algo:hex" representation.
func (d Digest) String() string { return d.Algorithm + ":" + d.Hex }

// Cache is a concurrency-safe on-disk content cache with a size limit and TTL
// based expiry. The in-memory index maps a relative file path to its size so
// that eviction can happen without walking the tree on every request.
type Cache struct {
	root    string
	maxSize int64
	log     *slog.Logger

	mu    sync.Mutex
	files map[string]int64
	bytes int64
}

// New creates (or opens) a cache rooted at root. maxSize is the soft upper bound
// in bytes; values <= 0 disable eviction.
func New(root string, maxSize int64, log *slog.Logger) (*Cache, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &Cache{root: root, maxSize: maxSize, log: log, files: make(map[string]int64)}
	for _, d := range []string{"blobs", "manifests", "refs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("cache: create %s: %w", d, err)
		}
	}
	if err := c.scan(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Cache) scan() error {
	return filepath.WalkDir(c.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(c.root, p)
		if rerr != nil {
			return nil
		}
		if strings.HasPrefix(rel, "tmp"+string(os.PathSeparator)) {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		c.files[rel] = fi.Size()
		c.bytes += fi.Size()
		return nil
	})
}

func (c *Cache) relBlob(d Digest) string {
	return filepath.Join("blobs", d.Algorithm, d.Hex)
}

func (c *Cache) relManifest(d Digest) string {
	return filepath.Join("manifests", d.Algorithm, d.Hex)
}

func (c *Cache) add(rel string, size int64) {
	c.mu.Lock()
	if old, ok := c.files[rel]; ok {
		c.bytes -= old
	}
	c.files[rel] = size
	c.bytes += size
	c.mu.Unlock()
}

func (c *Cache) remove(rel string) {
	c.mu.Lock()
	if old, ok := c.files[rel]; ok {
		c.bytes -= old
		delete(c.files, rel)
	}
	c.mu.Unlock()
}

func (c *Cache) touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// --- Blobs ---

// HasBlob reports whether the blob is present in the cache index.
func (c *Cache) HasBlob(d Digest) bool {
	c.mu.Lock()
	_, ok := c.files[c.relBlob(d)]
	c.mu.Unlock()
	return ok
}

// OpenBlob returns an open file for the blob and its size. The caller must
// close the file. It updates the access time used by LRU eviction.
func (c *Cache) OpenBlob(d Digest) (*os.File, int64, error) {
	p := filepath.Join(c.root, c.relBlob(d))
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	c.touch(p)
	return f, fi.Size(), nil
}

// PutBlob stores a blob read from r. It writes atomically via a temp file.
func (c *Cache) PutBlob(d Digest, r io.Reader) (int64, error) {
	tw, err := c.NewTempWriter()
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tw, r)
	if err != nil {
		tw.Abort()
		return 0, err
	}
	if err := tw.Commit(d); err != nil {
		return 0, err
	}
	return n, nil
}

// --- Manifests ---

// PutManifest stores manifest bytes and its media type.
func (c *Cache) PutManifest(d Digest, mediaType string, data []byte) error {
	p := filepath.Join(c.root, c.relManifest(d))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := atomicWrite(p, data); err != nil {
		return err
	}
	if mediaType != "" {
		if err := atomicWrite(p+".media", []byte(mediaType)); err != nil {
			return err
		}
		c.add(c.relManifest(d)+".media", int64(len(mediaType)))
	}
	c.add(c.relManifest(d), int64(len(data)))
	return nil
}

// GetManifest returns cached manifest bytes and media type.
func (c *Cache) GetManifest(d Digest) ([]byte, string, bool) {
	p := filepath.Join(c.root, c.relManifest(d))
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, "", false
	}
	media, _ := os.ReadFile(p + ".media")
	c.touch(p)
	return data, string(media), true
}

// HasManifest reports whether the manifest is cached.
func (c *Cache) HasManifest(d Digest) bool {
	_, err := os.Stat(filepath.Join(c.root, c.relManifest(d)))
	return err == nil
}

// --- Tag references ---

// PutRef records that ref in repository repo of upstream resolves to digest.
func (c *Cache) PutRef(upstream, repo, ref, digest string) error {
	p := c.refPath(upstream, repo, ref)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return atomicWrite(p, []byte(digest))
}

// GetRef resolves a cached tag to its manifest digest.
func (c *Cache) GetRef(upstream, repo, ref string) (string, bool) {
	b, err := os.ReadFile(c.refPath(upstream, repo, ref))
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(b))
	if _, err := ParseDigest(s); err != nil {
		return "", false
	}
	return s, true
}

// DeleteRef removes a cached tag reference.
func (c *Cache) DeleteRef(upstream, repo, ref string) {
	_ = os.Remove(c.refPath(upstream, repo, ref))
}

func (c *Cache) refPath(upstream, repo, ref string) string {
	return filepath.Join(c.root, "refs", safeSegment(upstream), safeRepoPath(repo), safeSegment(ref))
}

// --- Stats ---

// TotalBytes returns the current cache size in bytes.
func (c *Cache) TotalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// FileCount returns the number of indexed files.
func (c *Cache) FileCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.files)
}

// MaxBytes returns the configured size limit.
func (c *Cache) MaxBytes() int64 { return c.maxSize }

// Root returns the cache root directory.
func (c *Cache) Root() string { return c.root }

// --- TempWriter for streaming writes ---

// TempWriter writes into a temp file and can atomically commit the content as a
// blob with a known digest.
type TempWriter struct {
	c    *Cache
	f    *os.File
	done bool
}

// NewTempWriter creates a new temp file writer.
func (c *Cache) NewTempWriter() (*TempWriter, error) {
	f, err := os.CreateTemp(filepath.Join(c.root, "tmp"), "blob-*")
	if err != nil {
		return nil, err
	}
	return &TempWriter{c: c, f: f}, nil
}

func (t *TempWriter) Write(p []byte) (int, error) { return t.f.Write(p) }

// Abort discards the temp file.
func (t *TempWriter) Abort() {
	if t.done {
		return
	}
	t.done = true
	name := t.f.Name()
	_ = t.f.Close()
	_ = os.Remove(name)
}

// Commit atomically moves the temp file into place for digest d.
func (t *TempWriter) Commit(d Digest) error {
	if t.done {
		return errors.New("cache: temp writer already closed")
	}
	t.done = true
	if err := t.f.Sync(); err != nil {
		_ = t.f.Close()
		_ = os.Remove(t.f.Name())
		return err
	}
	fi, err := t.f.Stat()
	if err != nil {
		_ = t.f.Close()
		_ = os.Remove(t.f.Name())
		return err
	}
	if err := t.f.Close(); err != nil {
		_ = os.Remove(t.f.Name())
		return err
	}
	dest := filepath.Join(t.c.root, t.c.relBlob(d))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		_ = os.Remove(t.f.Name())
		return err
	}
	if err := os.Rename(t.f.Name(), dest); err != nil {
		_ = os.Remove(t.f.Name())
		return err
	}
	t.c.add(t.c.relBlob(d), fi.Size())
	t.c.evict()
	return nil
}

// --- Eviction and expiry ---

// StartCleaner runs periodic TTL expiry and LRU eviction until ctx is done.
func (c *Cache) StartCleaner(ctx context.Context, interval, blobTTL, manifestTTL time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.clean(blobTTL, manifestTTL)
			c.evict()
		}
	}
}

func (c *Cache) clean(blobTTL, manifestTTL time.Duration) {
	now := time.Now()
	var removed int
	c.mu.Lock()
	for rel, size := range c.files {
		var ttl time.Duration
		switch {
		case strings.HasPrefix(rel, "blobs"+string(os.PathSeparator)):
			ttl = blobTTL
		case strings.HasPrefix(rel, "manifests"+string(os.PathSeparator)):
			ttl = manifestTTL
		default:
			continue
		}
		if ttl <= 0 {
			continue
		}
		fi, err := os.Stat(filepath.Join(c.root, rel))
		if err != nil {
			c.bytes -= size
			delete(c.files, rel)
			continue
		}
		if now.Sub(fi.ModTime()) > ttl {
			if os.Remove(filepath.Join(c.root, rel)) == nil {
				c.bytes -= size
				delete(c.files, rel)
				removed++
			}
		}
	}
	c.mu.Unlock()
	if removed > 0 {
		c.log.Info("cache expiry removed files", "count", removed, "sizeBytes", c.TotalBytes())
	}
}

// evict removes least-recently-modified entries until the cache is back under
// ~90% of maxSize.
func (c *Cache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxSize <= 0 || c.bytes <= c.maxSize {
		return
	}
	type item struct {
		rel  string
		size int64
		mod  time.Time
	}
	list := make([]item, 0, len(c.files))
	for rel, size := range c.files {
		fi, err := os.Stat(filepath.Join(c.root, rel))
		if err != nil {
			c.bytes -= size
			delete(c.files, rel)
			continue
		}
		list = append(list, item{rel, size, fi.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.Before(list[j].mod) })
	target := c.maxSize - c.maxSize/10
	var freed int64
	for _, it := range list {
		if c.bytes <= target {
			break
		}
		if err := os.Remove(filepath.Join(c.root, it.rel)); err == nil || errors.Is(err, os.ErrNotExist) {
			c.bytes -= it.size
			delete(c.files, it.rel)
			freed += it.size
		}
	}
	if freed > 0 {
		c.log.Info("cache eviction freed space", "freedBytes", freed, "sizeBytes", c.bytes, "maxBytes", c.maxSize)
	}
}

// --- helpers ---

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// safeSegment makes a single path segment filesystem-safe and traversal-proof.
func safeSegment(s string) string {
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func safeRepoPath(repo string) string {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	for i, p := range parts {
		parts[i] = safeSegment(p)
	}
	return filepath.Join(parts...)
}

// HashReader returns the sha256 hex digest of the bytes read from r.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
