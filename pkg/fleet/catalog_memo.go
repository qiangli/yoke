package fleet

import (
	"slices"
	"strconv"
	"sync"

	"github.com/qiangli/yoke/pkg/assetring"
)

// catalogMemo remembers the parsed rows of each noun for one Catalog.
//
// Why it exists: every lookup (Tool, Model, Agent, Binding) was a full
// re-read and re-parse of its rings, and the fleet's hot loops call those
// hundreds of times per agent. A sprint assignment over a 211-binding
// roster parsed the same 18 tool files a few thousand times and spent two
// minutes of CPU picking an agent (yoke todo 1875c7c3).
//
// Why it is safe: the memo is keyed by the rings' fingerprint — every
// entry's name, size, mode and modification time, taken fresh on each call
// (assetring.Fingerprint). A write through this package, a test's direct
// os.WriteFile, or another process editing the store all change the key,
// so there is no invalidation protocol to get wrong. A noun whose sources
// cannot fingerprint themselves is never memoized. The cost of a hit is a
// directory listing with one stat per entry, which is two orders of
// magnitude under a parse.
type catalogMemo struct {
	mu      sync.Mutex
	entries map[string]memoEntry
	loads   int // rebuilds, counted so a test can tell a hit from a miss
}

// sharedMemo is the process-wide store behind every Catalog built from the
// compiled-in baseline with no injected sources — which is every production
// catalog. Callers build a fresh Catalog per lookup site (fleet.New() is
// cheap by design), and the sprint seat check does so once per candidate,
// so a per-instance memo started cold a few hundred times per command. The
// fingerprint check runs on every hit, so sharing changes nothing about
// freshness: a ring edited through one instance is seen by all of them.
// Catalogs with a test baseline or injected sources keep an instance memo,
// since their world is not addressable by root alone.
var sharedMemo = struct {
	mu      sync.Mutex
	entries map[string]memoEntry
}{entries: map[string]memoEntry{}}

// memoScope names the shared store slot for this catalog, or reports false
// when the catalog must keep its memo to itself.
func (c *Catalog) memoScope() (string, bool) {
	if c.cfg.baseFS != nil || len(c.cfg.shared) > 0 {
		return "", false
	}
	return c.cfg.root + "|" + strconv.FormatBool(c.cfg.rootSet) + "|" + strconv.FormatBool(c.cfg.noLocal) + "|" + strconv.FormatBool(c.cfg.noCloud), true
}

type memoEntry struct {
	fingerprint string
	value       any
	errs        []error
}

func newCatalogMemo() *catalogMemo {
	return &catalogMemo{entries: map[string]memoEntry{}}
}

// memoized returns the cached value for key when the named nouns' rings
// are unchanged, and rebuilds it with load otherwise. Slices come back as
// shallow copies, so a caller appending to or reordering the result cannot
// disturb the next caller's copy.
func memoized[T any](c *Catalog, key string, nouns []string, load func() ([]T, []error)) ([]T, []error) {
	if c.memo == nil {
		return load()
	}
	fingerprint, ok := c.fingerprint(nouns)
	if !ok {
		return load()
	}
	mu, entries := &c.memo.mu, c.memo.entries
	if scope, ok := c.memoScope(); ok {
		mu, entries, key = &sharedMemo.mu, sharedMemo.entries, scope+"/"+key
	}
	mu.Lock()
	e, hit := entries[key]
	mu.Unlock()
	if hit && e.fingerprint == fingerprint {
		if v, ok := e.value.([]T); ok {
			return slices.Clone(v), slices.Clone(e.errs)
		}
	}
	v, errs := load()
	mu.Lock()
	entries[key] = memoEntry{fingerprint: fingerprint, value: v, errs: errs}
	mu.Unlock()
	c.memo.mu.Lock()
	c.memo.loads++
	c.memo.mu.Unlock()
	return slices.Clone(v), slices.Clone(errs)
}

// fingerprint combines the ring fingerprints of several nouns, for a value
// derived from more than one (agents are decorated with models and tools).
func (c *Catalog) fingerprint(nouns []string) (string, bool) {
	var all string
	for _, noun := range nouns {
		tok, ok := assetring.Fingerprint(c.sources(noun))
		if !ok {
			return "", false
		}
		all += noun + "=" + tok + "|"
	}
	return all, true
}
