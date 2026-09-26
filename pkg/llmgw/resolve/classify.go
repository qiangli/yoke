package resolve

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tier scale. Buckets are inclusive at the upper bound of the
// previous tier and exclusive at the upper bound of their own —
// i.e. 3B-param models fall in TierL2, 13B in TierL3, 35B in TierL4.
const (
	TierUnknown = 0
	TierL1      = 1 // < 3B params
	TierL2      = 2 // 3B - 13B
	TierL3      = 3 // 13B - 35B
	TierL4      = 4 // >= 35B
)

// Domain labels, multi-label per model. A model can be in several
// (e.g. qwen2.5-coder:7b is {coding, general}). "general" is the
// default for instruct/chat models with no specialisation signal;
// when other domains apply, "general" is dropped from the set so the
// resolver doesn't substitute a general model for a coding request.
const (
	DomainEmbedding = "embedding"
	DomainVision    = "vision"
	DomainCoding    = "coding"
	DomainReasoning = "reasoning"
	DomainGeneral   = "general"
)

// AllDomains is the canonical ordering returned by ClassifyModel
// (sorted by specificity, then alphabetical). Used for stable test
// output and for the multi-label string form.
var AllDomains = []string{DomainEmbedding, DomainVision, DomainCoding, DomainReasoning, DomainGeneral}

// paramSizeRE pulls a parameter count out of "1B", "7B", "13b",
// "8x7B", "0.5B", "1.5B" etc. The expansion-shape "8x7B" reports the
// per-expert size — for routing-tier purposes the per-expert size is
// the right cost signal (MoE only activates one expert at a time
// during inference).
var paramSizeRE = regexp.MustCompile(`(?i)(?:[0-9]+x)?([0-9]+(?:\.[0-9]+)?)\s*[bm]\b`)

// nameTagRE plucks a `:<size>` suffix off a model name like
// "llama3.2:3b" or "qwen2.5-coder:32b". Used as a fallback when
// ParameterSize wasn't reported (backends that pre-date the
// /api/show enrichment).
var nameTagRE = regexp.MustCompile(`(?i):\s*([0-9]+(?:\.[0-9]+)?)\s*[bm]\b`)

// ClassifyModel returns the (tier, domains) for one model, derived
// from its name, family, parameter-size string, and the capabilities
// list the backend reports. All inputs are best-effort: an empty
// family or missing param size still produces a usable answer via
// name regex.
//
// The classifier is deterministic and pure — no overrides here; a
// catalog can layer them on top. Empty (Tier=0, Domain=nil) is
// returned only when the model name is itself empty; otherwise we
// always emit at least a Tier guess and at least DomainGeneral.
func ClassifyModel(name, family, paramSize string, capabilities []string) (tier int, domains []string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TierUnknown, nil
	}
	tier = tierFromParams(paramSize, name)
	domains = domainsFor(name, family, capabilities)
	return tier, domains
}

// tierFromParams parses the model's parameter count and maps it onto
// L1..L4. Falls back to a name-tag scan when paramSize is empty or
// unparseable (older backends). Last resort: TierL2, the "default
// model" tier and the safest guess for an unknown-sized chat model.
func tierFromParams(paramSize, name string) int {
	billions := parseParamsBillions(paramSize)
	if billions <= 0 {
		billions = parseNameTagBillions(name)
	}
	switch {
	case billions <= 0:
		return TierL2
	case billions < 3.0:
		return TierL1
	case billions < 13.0:
		return TierL2
	case billions < 35.0:
		return TierL3
	default:
		return TierL4
	}
}

// parseParamsBillions extracts a billions-of-parameters float from a
// backend's "parameter_size" string. Handles "7B", "0.5B", "1.5B",
// "8x7B" (MoE — per-expert size), and bare numbers ("7000000000" →
// 7.0). Returns 0 on unparseable / empty.
func parseParamsBillions(paramSize string) float64 {
	s := strings.TrimSpace(paramSize)
	if s == "" {
		return 0
	}
	if m := paramSizeRE.FindStringSubmatch(s); len(m) >= 2 {
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0
		}
		if strings.Contains(strings.ToLower(s), "m") && !strings.Contains(strings.ToLower(s), "b") {
			f = f / 1000.0
		}
		return f
	}
	// Bare number (no unit) — assume raw param count.
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return n / 1e9
	}
	return 0
}

// parseNameTagBillions falls back to "qwen2.5-coder:7b" → 7.0 style
// extraction when paramSize wasn't reported. Conservative: only
// matches the `:<size>` suffix shape, not arbitrary occurrences of
// "7b" in the model name.
func parseNameTagBillions(name string) float64 {
	if m := nameTagRE.FindStringSubmatch(name); len(m) >= 2 {
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0
		}
		if strings.Contains(strings.ToLower(m[0]), "m") {
			f = f / 1000.0
		}
		return f
	}
	return 0
}

var (
	// Domain regexes. Multi-label: more than one may match.
	embeddingNameRE = regexp.MustCompile(`(?i)(^|[-/_])(embed|nomic-embed|bge-|mxbai-embed|gte-|jina-embed|snowflake-arctic-embed)`)
	visionNameRE    = regexp.MustCompile(`(?i)(^|[-/_])(llava|llama-?vision|llama3\.2-vision|vision|moondream|cogvlm|minicpm-v)`)
	codingNameRE    = regexp.MustCompile(`(?i)(^|[-/_])(coder?|codellama|deepseek-coder|starcoder|codestral|codegemma|qwen.*coder|wizardcoder|granite-code)`)
	reasoningNameRE = regexp.MustCompile(`(?i)(^|[-/_])(r1\b|reasoner|marco-o1|qwq|phi-?4-?reasoning|qwen3?-?thinking|deepseek-r1)`)
)

// domainsFor scores the requested model against the known domains.
// Capabilities (from the backend's model-show endpoint) win when they
// unambiguously signal a domain — e.g. an "embedding" cap forces
// DomainEmbedding regardless of the name. Name and family regexes
// fill in when capabilities are silent (which is most of ollama 0.5-
// and many community models).
//
// Returns at least DomainGeneral. If a specialised domain matches,
// DomainGeneral is dropped — substituting a vanilla chat model for a
// coding request is the wrong call.
func domainsFor(name, family string, capabilities []string) []string {
	set := map[string]struct{}{}
	add := func(d string) { set[d] = struct{}{} }

	for _, c := range capabilities {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "embedding", "embed":
			add(DomainEmbedding)
		case "vision":
			add(DomainVision)
		}
	}

	lname := strings.ToLower(name)
	lfamily := strings.ToLower(strings.TrimSpace(family))

	if embeddingNameRE.MatchString(lname) || lfamily == "bert" || strings.Contains(lfamily, "embed") {
		add(DomainEmbedding)
	}
	if visionNameRE.MatchString(lname) || strings.Contains(lfamily, "vision") || strings.Contains(lfamily, "llava") {
		add(DomainVision)
	}
	if codingNameRE.MatchString(lname) || strings.Contains(lfamily, "coder") || strings.Contains(lfamily, "starcoder") {
		add(DomainCoding)
	}
	if reasoningNameRE.MatchString(lname) {
		add(DomainReasoning)
	}

	// If no specialised domain found, mark as general. Otherwise drop
	// general so the substitution rules above don't pick a vanilla
	// chat model in place of e.g. a coding model.
	if _, special := anyOf(set, DomainEmbedding, DomainVision, DomainCoding, DomainReasoning); !special {
		add(DomainGeneral)
	}

	out := make([]string, 0, len(set))
	for _, d := range AllDomains {
		if _, ok := set[d]; ok {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return indexOf(AllDomains, out[i]) < indexOf(AllDomains, out[j])
	})
	return out
}

func anyOf(set map[string]struct{}, keys ...string) (string, bool) {
	for _, k := range keys {
		if _, ok := set[k]; ok {
			return k, true
		}
	}
	return "", false
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return math.MaxInt
}
