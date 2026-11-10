// Package llmprice is yoke's single table of per-token model prices, in USD
// per million tokens.
//
// It is deliberately stdlib-only and dependency-free: the door (pkg/broker)
// writes a cost on every audit record, and ycode is meant to import this very
// table later, so it must not grow a dependency on the fleet catalog, the
// telemetry stack or anything else that would drag along.
//
// Seeded from ycode internal/runtime/usage/pricing.go, with two corrections
// this sprint's metering story (Sprint #602 A2) requires:
//
//   - An unknown model is UNKNOWN. The seed table answered every model it had
//     never heard of with Claude Sonnet's rate ($3/$15 per M) and a Known
//     flag nobody downstream read. GLM-5.2 was billed at 5x its API price
//     that way. Here Lookup returns Known=false and zero rates, and callers
//     that need a dollar figure must check Known first: the door never falls
//     back to another model's price.
//   - Bedrock ids normalize. Bedrock spells a model
//     "<region>.<vendor>.<name>" (us.anthropic.claude-opus-4-8,
//     eu.anthropic.claude-sonnet-4-6, zai.glm-5, openai.gpt-6-sol,
//     amazon.nova-lite-v1) and a tool may hand the door any of those
//     spellings, its own provider-prefixed spelling
//     (amazon-bedrock/zai.glm-5), or the bare name. NormalizeModel strips
//     the region and vendor prefixes and the provider path, so one table row
//     prices every spelling of the same weights.
//
// Prices are USD per million tokens (input, output, cache write, cache read).
// A zero rate that the table declared is a price: it means "this item is free
// on this model" (OpenAI has no cache-write charge). A zero struct the table
// never declared means "we do not know".
package llmprice

import (
	"regexp"
	"strings"
	"sync"
)

// Pricing is the per-token price of one model, in USD per million tokens.
//
// Known is the whole contract: it is false exactly when the table has no row
// for the model, in which case every rate is zero and quoting any of them as
// a cost would be a guess dressed up as a measurement. A caller reporting a
// cost must carry Known through — that is what `unpriced` on the door's audit
// record is.
type Pricing struct {
	InputPerM      float64
	OutputPerM     float64
	CacheWritePerM float64
	CacheReadPerM  float64

	// Known reports whether the table declared a price for this model.
	Known bool
}

// Usage is the token accounting one request is priced against. Cached-input
// tokens (a cache read) and cache-creation tokens (a cache write) are counted
// separately from the uncached input, so the price of a warm turn is not
// mistaken for the price of a cold one.
type Usage struct {
	Input      int64 // uncached prompt tokens
	Output     int64 // completion tokens
	CacheWrite int64 // cache creation (prompt caching write) tokens
	CacheRead  int64 // cache read (cached prompt) tokens
}

// Tokens reports the total token count, cached or not. It is what a usage
// report shows next to the cost, and it is not part of the price.
func (u Usage) Tokens() int64 {
	return u.Input + u.Output + u.CacheWrite + u.CacheRead
}

// Table prices models by normalized id. Keys are what NormalizeModel returns:
// lowercase, provider path and Bedrock region/vendor prefixes stripped, ":latest"
// style tags dropped. A Table with no Models answers Known=false for
// everything, which is the honest answer, never a fallback price.
type Table struct {
	Models map[string]Pricing
}

// Default is the built-in price table. It is data plus the normalization rule:
// adding a model means adding a row (and a test pinning the number), never a
// code change, and it never fetches a provider catalog.
var Default = DefaultTable()

// defaultTable is built once. The map is published read-only; Register is the
// only writer and holds the lock, so a reader never observes a torn map.
var (
	defaultMu     sync.RWMutex
	defaultBuilt  bool
	defaultModels = map[string]Pricing{}
)

// DefaultTable returns the built-in table. It is safe to call concurrently
// and returns the same table every time.
func DefaultTable() *Table {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if !defaultBuilt {
		for id, p := range seedModels() {
			defaultModels[id] = p
		}
		defaultBuilt = true
	}
	return &Table{Models: defaultModels}
}

// Register adds or replaces rows in the default table at run time (an
// operator overlay, or a provider catalog a deployment chooses to trust). It
// cannot make an unknown model priced by accident: the rows must name the
// models they price. Returns the table for chaining.
func Register(models map[string]Pricing) *Table {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	for id, p := range models {
		defaultModels[NormalizeModel(id)] = p
	}
	return &Table{Models: defaultModels}
}

// Lookup prices one model id, as any surface spelled it. Unknown means
// unknown: Known=false with zero rates, never another model's price.
func (t *Table) Lookup(model string) Pricing {
	if p, ok := t.Models[NormalizeModel(model)]; ok {
		return p
	}
	return Pricing{}
}

// Lookup prices one model id against the default table.
func Lookup(model string) Pricing { return Default.Lookup(model) }

// Known reports whether the table prices the model in any of its spellings.
func (t *Table) Known(model string) bool { return t.Lookup(model).Known }

// Cost prices one usage record against one model. The second return is false
// when the model is not in the table — the caller must not report the zero as
// a cost, because zero would read as "this request was free".
func (t *Table) Cost(model string, u Usage) (float64, bool) {
	p := t.Lookup(model)
	if !p.Known {
		return 0, false
	}
	return (float64(u.Input)*p.InputPerM +
		float64(u.Output)*p.OutputPerM +
		float64(u.CacheWrite)*p.CacheWritePerM +
		float64(u.CacheRead)*p.CacheReadPerM) / 1_000_000, true
}

// Cost prices one usage record against the default table.
func Cost(model string, u Usage) (float64, bool) { return Default.Cost(model, u) }

// ---- id normalization ------------------------------------------------------

// crossRegionProfiles are the Bedrock cross-region inference-profile
// prefixes ("us.", "eu.", "apac.", "global."). They carry no price
// information: the same profile id is priced the same under every one.
var crossRegionProfiles = map[string]bool{
	"us": true, "eu": true, "apac": true, "global": true,
}

// singleRegionRE matches a single-region Bedrock partition prefix
// ("us-east-1.", "us-gov-west-1.", "eu-central-2.", "cn-north-1.",
// "ap-southeast-7."). Every AWS region ends in a digit, which is what keeps
// a version number ("glm-5.3") from reading as a region.
var singleRegionRE = regexp.MustCompile(`^[a-z]{2}(-gov)?(-[a-z]+)+-[0-9]+$`)

// bedrockVendors are the vendor namespaces Bedrock puts between the optional
// region prefix and the model name (anthropic., openai., zai., meta.,
// amazon., cohere., ai21., mistral., stability., deepseek., qwen.). A region
// prefix makes the next component a vendor by Bedrock's own shape, so an id
// carrying a region does not need the vendor to be one of these; this set
// covers the region-less spellings ("zai.glm-5", "amazon.nova-pro-v1").
//
// "amazon-bedrock" is opencode's own provider spelling of the same route and
// is stripped by the provider-path rule below.
var bedrockVendors = map[string]bool{
	"anthropic": true, "openai": true, "zai": true, "meta": true, "amazon": true,
	"cohere": true, "ai21": true, "mistral": true, "stability": true,
	"deepseek": true, "qwen": true, "writer": true, "perplexity": true,
}

func isRegionPrefix(s string) bool {
	return crossRegionProfiles[s] || singleRegionRE.MatchString(s)
}

// NormalizeModel reduces every spelling of one model's id to one table key:
// lowercase, whitespace trimmed, any "<provider>/" path prefix dropped
// (moonshot/, deepseek/, zai-coding-plan/, amazon-bedrock/, openai-compat/),
// a Bedrock "<region>.<vendor>." prefix dropped, then any remaining
// "<vendor>." prefix dropped, and an Ollama ":latest" style tag dropped.
//
//	us.anthropic.claude-opus-4-8      -> claude-opus-4-8
//	eu.anthropic.claude-sonnet-4-6    -> claude-sonnet-4-6
//	zai.glm-5                         -> glm-5
//	openai.gpt-6-sol                  -> gpt-6-sol
//	amazon.nova-lite-v1               -> nova-lite-v1
//	amazon-bedrock/zai.glm-5          -> glm-5
//	moonshot/kimi-k2.7-code           -> kimi-k2.7-code
//	zai-coding-plan/glm-5.3           -> glm-5.3
//	qwen2.5-coder:latest              -> qwen2.5-coder
//	qwen/qwen2.5-coder                -> qwen2.5-coder
//
// It never strips a prefix that is part of a bare model name ("qwen2.5-coder"
// keeps its qwen), and it never returns an empty string for a non-empty
// input: an id that is nothing but prefixes ("anthropic.") stays as it is
// rather than becoming a key that would price every unprefixed model.
func NormalizeModel(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	if s == "" {
		return ""
	}
	// Drop a provider path prefix: everything before the last "/".
	if _, name, found := strings.Cut(s, "/"); found {
		s = name
	}
	// Drop an Ollama ":tag" suffix ("llama3:latest").
	if name, _, found := strings.Cut(s, ":"); found {
		s = name
	}
	// Bedrock: "<region>.<vendor>.<name>" or "<vendor>.<name>". A region
	// prefix makes the following component the vendor namespace by Bedrock's
	// own shape, so it is dropped unconditionally; without a region prefix
	// only a vendor this table knows is dropped, which is what keeps a
	// version number ("glm-5.3") or a bare model name ("qwen2.5-coder")
	// intact.
	parts := strings.Split(s, ".")
	drop := 0
	switch {
	case isRegionPrefix(parts[0]):
		switch len(parts) {
		case 1: // "us-east-1" alone: the id IS a region, keep it
			drop = 0
		case 2: // "us-east-1.model": no vendor namespace between them
			drop = 1
		default: // the region prefix and its vendor namespace
			drop = 2
		}
	case len(parts) > 1 && bedrockVendors[parts[0]]:
		drop = 1
	}
	return strings.Join(parts[drop:], ".")
}
