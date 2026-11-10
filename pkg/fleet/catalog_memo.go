package fleet

import (
	"slices"
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
	c.memo.mu.Lock()
	e, hit := c.memo.entries[key]
	c.memo.mu.Unlock()
	if hit && e.fingerprint == fingerprint {
		if v, ok := e.value.([]T); ok {
			return slices.Clone(v), slices.Clone(e.errs)
		}
	}
	v, errs := load()
	c.memo.mu.Lock()
	c.memo.entries[key] = memoEntry{fingerprint: fingerprint, value: v, errs: errs}
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
