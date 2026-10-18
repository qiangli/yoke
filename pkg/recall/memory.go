package recall

import (
	"fmt"
	"slices"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
)

// --memory vocabulary (design §3). Each memory type is sugar over the facets
// that already exist — ring · form · Type · episode — never a stored field.
const (
	MemoryWorking    = "working"
	MemoryEpisodic   = "episodic"
	MemorySemantic   = "semantic"
	MemoryProcedural = "procedural"
	MemoryReflective = "reflective"
	MemoryProfile    = "profile"
	MemoryCode       = "code"
)

// MemoryTypes is the --memory vocabulary, in the order the unknown-value
// error lists it.
func MemoryTypes() []string {
	return []string{
		MemoryWorking, MemoryEpisodic, MemorySemantic, MemoryProcedural,
		MemoryReflective, MemoryProfile, MemoryCode,
	}
}

// memoryFacets is one row of the §3 expansion table: the default facet
// selection a --memory TYPE stands for. A nil rings/forms/types means the
// row does not constrain that facet (the caller's selection, or every ring
// when unset). foreignRefs means the row's records live outside every kb
// ring — behind episode: refs in M1 — so no kb reader is consulted.
type memoryFacets struct {
	rings       []string
	forms       []string
	types       []string
	foreignRefs bool
}

// expandMemory maps a --memory TYPE to its §3 facet combination. The table
// below is the contract; the per-row unit test pins every line of it.
func expandMemory(memory string) (memoryFacets, error) {
	switch normalizeMemory(memory) {
	case MemoryWorking:
		// Checkpoint notes on the agent ring for exactly --episode.
		return memoryFacets{
			rings: []string{RingAgent},
			forms: []string{kb.FormNote},
		}, nil
	case MemoryEpisodic:
		// Execlog records + observe journal events, addressed by episode:
		// ref for --episode (indexed, never ingested). No kb reader serves
		// them in M1; full content is `bashy graph history --episode ID`.
		return memoryFacets{foreignRefs: true}, nil
	case MemorySemantic:
		return memoryFacets{
			rings: []string{RingAgent, RingRepo, RingHost},
			forms: []string{kb.FormPage, kb.FormNote, kb.FormRelation},
			types: []string{kb.TypeFact, kb.TypeDecision},
		}, nil
	case MemoryProcedural:
		// Runbook pages everywhere plus the capability ring's craft folds
		// (facts are never exported). Folds carry no OKF page type, so the
		// type filter below applies to kb pages only.
		return memoryFacets{
			rings: []string{RingAgent, RingRepo, RingHost, RingCapability},
			forms: []string{kb.FormPage, kb.FormNote},
			types: []string{kb.TypeRunbook},
		}, nil
	case MemoryReflective:
		return memoryFacets{
			rings: []string{RingAgent, RingRepo, RingHost},
			forms: []string{kb.FormPage, kb.FormNote},
			types: []string{kb.TypeLesson, kb.TypeGotcha},
		}, nil
	case MemoryProfile:
		return memoryFacets{
			rings: []string{RingHost, RingAgent},
			forms: []string{kb.FormPage, kb.FormNote},
			types: []string{kb.TypeFact, kb.TypeDecision},
		}, nil
	case MemoryCode:
		// Exactly --forms code: the read-only code-graph view plus the
		// budgeted repomap, served by the injected CodeRing reader.
		return memoryFacets{
			forms: []string{kb.FormCode},
		}, nil
	default:
		return memoryFacets{}, fmt.Errorf("kb: unknown memory type %q (working|episodic|semantic|procedural|reflective|profile|code)", memory)
	}
}

func normalizeMemory(memory string) string {
	return strings.ToLower(strings.TrimSpace(memory))
}

// ApplyMemory folds Query.Memory into effective Rings/Forms/Types and reports
// an unknown --memory value as an error. It is idempotent: a query it already
// resolved is returned unchanged, so commands may resolve before calling
// Recall/Context (which resolve again for direct library callers).
//
// --memory sets the defaults; an explicit Rings/Forms narrows the expansion
// by intersection. An empty intersection matches nothing — the normal empty
// answer, never an error. Episode passes through untouched: working reads
// exactly --episode, and the checkpoint gate already narrows every other row
// to the named episode.
func ApplyMemory(q Query) (Query, error) {
	if q.memoryRingsStrict || q.memoryFormsStrict || strings.TrimSpace(q.Memory) == "" {
		return q, nil
	}
	exp, err := expandMemory(q.Memory)
	if err != nil {
		return q, err
	}
	q.Memory = normalizeMemory(q.Memory)
	if exp.foreignRefs {
		// Episodic records are foreign refs, not kb facets: consult no
		// ring, whatever the caller named. The envelope still echoes the
		// filter, so the answer reads as empty, not as broken.
		q.Rings, q.Forms, q.Types = nil, nil, nil
		q.memoryRingsStrict, q.memoryFormsStrict = true, true
		return q, nil
	}
	// A nil row leaves the facet alone: the code row names no rings, so an
	// unset Rings still means every reader. A non-nil row intersects, and an
	// empty intersection matches nothing — the normal empty answer.
	if exp.rings != nil {
		q.Rings = intersectFacet(q.Rings, exp.rings)
		q.memoryRingsStrict = true
	}
	if exp.forms != nil {
		q.Forms = intersectFacet(q.Forms, exp.forms)
		q.memoryFormsStrict = true
	}
	q.Types = intersectFacet(q.Types, exp.types)
	return q, nil
}

// intersectFacet narrows an explicit facet selection by an expansion row. An
// empty explicit selection takes the row's defaults; a nil row leaves the
// explicit selection (possibly every ring) alone. Overlap follows the row's
// order, which is canonical and deterministic.
func intersectFacet(explicit, row []string) []string {
	if row == nil {
		return explicit
	}
	if len(explicit) == 0 {
		return append([]string(nil), row...)
	}
	var out []string
	for _, f := range row {
		if slices.Contains(explicit, f) {
			out = append(out, f)
		}
	}
	return out
}

// allowsRing reports whether a ring is selected. After --memory resolution
// an empty Rings means the intersection was empty (match nothing) — unless
// the memory row left rings unconstrained (code), in which case empty keeps
// the historical meaning of every ring.
func (q Query) allowsRing(ring string) bool {
	if q.memoryRingsStrict {
		return slices.Contains(q.Rings, ring)
	}
	if len(q.Rings) == 0 {
		return true
	}
	return slices.Contains(q.Rings, ring)
}

// allowsForm is the form twin of allowsRing.
func (q Query) allowsForm(form string) bool {
	if q.memoryFormsStrict {
		return slices.Contains(q.Forms, form)
	}
	if len(q.Forms) == 0 {
		return true
	}
	return slices.Contains(q.Forms, form)
}
