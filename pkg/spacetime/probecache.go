package spacetime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cache persists lazy probe values across processes. Implementations
// must treat a PATH-hash mismatch as a miss (a changed PATH can change
// every tool resolution).
//
// Volatile namespaces never reach a Cache — see VolatileResolver.
type Cache interface {
	Get(name, pathHash string) (val string, ok bool)
	Put(name, pathHash, val string)
}

// NopCache caches nothing (tests, --refresh).
func NopCache() Cache { return nopCache{} }

type nopCache struct{}

func (nopCache) Get(string, string) (string, bool) { return "", false }
func (nopCache) Put(string, string, string)        {}

// FileCache persists probe values in <dir>/probecache.json with a TTL.
// A corrupt or stale file is silently dropped and rebuilt — the cache
// must never fail a verb.
type FileCache struct {
	mu   sync.Mutex
	path string
	ttl  time.Duration
	now  func() time.Time // test seam
}

func NewFileCache(dir string, ttl time.Duration) *FileCache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &FileCache{path: filepath.Join(dir, "probecache.json"), ttl: ttl, now: time.Now}
}

// SetNow overrides the clock. Test seam.
func (c *FileCache) SetNow(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

type cacheFile struct {
	PathHash string                `json:"path_hash"`
	Entries  map[string]cacheEntry `json:"entries"`
}

type cacheEntry struct {
	V string    `json:"v"`
	T time.Time `json:"t"`
}

func (c *FileCache) load(pathHash string) cacheFile {
	f := cacheFile{PathHash: pathHash, Entries: map[string]cacheEntry{}}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return f
	}
	var got cacheFile
	if json.Unmarshal(data, &got) != nil || got.PathHash != pathHash || got.Entries == nil {
		return f
	}
	return got
}

func (c *FileCache) Get(name, pathHash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.load(pathHash).Entries[name]
	if !ok || c.now().Sub(e.T) > c.ttl {
		return "", false
	}
	return e.V, true
}

func (c *FileCache) Put(name, pathHash, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.load(pathHash)
	f.Entries[name] = cacheEntry{V: val, T: c.now()}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(c.path, data, 0o644)
}

// Entries returns the still-valid cached values for a PATH hash — used
// by `skills probe` to display lazy probes without re-evaluating them.
func (c *FileCache) Entries(pathHash string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.load(pathHash)
	out := make(map[string]string, len(f.Entries))
	for k, e := range f.Entries {
		if c.now().Sub(e.T) <= c.ttl {
			out[k] = e.V
		}
	}
	return out
}
