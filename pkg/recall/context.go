package recall

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
)

// ContextVersion is the frozen kb-context envelope version.
const ContextVersion = 1

// ContextResult is the one budgeted assembler envelope. Fields may be added,
// never removed or repurposed; testdata/kb-context-envelope.json pins it.
type ContextResult struct {
	ContextVersion int            `json:"context_version"`
	For            string         `json:"for"`
	Memory         string         `json:"memory,omitempty"`
	Budget         ContextBudget  `json:"budget"`
	Abstained      bool           `json:"abstained"`
	Rings          []ContextRing  `json:"rings"`
	Blocks         []ContextBlock `json:"blocks"`
}

type ContextBudget struct {
	Limit int `json:"limit"`
	Used  int `json:"used"`
}

type ContextRing struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type ContextBlock struct {
	Ring       string `json:"ring"`
	Form       string `json:"form"`
	Ref        string `json:"ref"`
	Memory     string `json:"memory,omitempty"`
	Tokens     int    `json:"tokens"`
	Resolution string `json:"resolution,omitempty"`
	Status     string `json:"status,omitempty"`
	// ValidTo is the RFC3339 end of validity, written by M6 supersede and
	// read through here: M1 emits it only when the source record carries one.
	ValidTo string `json:"valid_to,omitempty"`
	// ObservedAt, Freshness and Snapshot are the sense-observation surface
	// (design §5.1): source ref, observed_at, freshness/status and snapshot
	// digest. ObservedAt is copied from the hit whenever the source carries
	// one; freshness and snapshot stay empty until the sense schema lands.
	ObservedAt string `json:"observed_at,omitempty"`
	Freshness  string `json:"freshness,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	// Path is reserved for M4 --hops provenance (seed → relation → node).
	// M1 reserves the name and never emits it.
	Path   []string    `json:"path,omitempty"`
	Text   string      `json:"text"`
	Why    []string    `json:"why,omitempty"`
	Source []SourceRef `json:"source,omitempty"`

	// working is internal ordering state (never emitted): see workingLast.
	working bool
}

// Context ranks within each reader, preserves ring boundaries, then spends one
// global budget on presentation. It first reserves the cheapest cue for every
// hit that fits; only then does it upgrade each block to the largest resolution
// that fits. A high-ranked full page therefore cannot starve every later ring.
func Context(q Query, readers ...Reader) ContextResult {
	rq, merr := ApplyMemory(q)
	if merr != nil {
		return ContextResult{
			ContextVersion: ContextVersion,
			For:            q.Text,
			Memory:         normalizeMemory(q.Memory),
			Budget:         ContextBudget{Limit: q.Budget},
			Blocks:         []ContextBlock{},
			Rings:          []ContextRing{{Name: "memory", OK: false, Error: merr.Error()}},
		}
	}
	q = rq
	res := ContextResult{
		ContextVersion: ContextVersion,
		For:            q.Text,
		Memory:         q.Memory,
		Budget:         ContextBudget{Limit: q.Budget},
		Blocks:         []ContextBlock{},
	}
	ordered := orderedReaders(q, readers)
	statusIndex := map[string]int{}
	for _, rd := range ordered {
		if _, ok := statusIndex[rd.Ring()]; !ok {
			statusIndex[rd.Ring()] = len(res.Rings)
			res.Rings = append(res.Rings, ContextRing{Name: rd.Ring(), OK: true})
		}
	}

	byRing := map[string][]Hit{}
	for _, rd := range ordered {
		if !readerIntersectsForms(rd, q) {
			continue
		}
		ringHits, err := rd.Recall(q)
		if err != nil {
			i := statusIndex[rd.Ring()]
			res.Rings[i].OK = false
			if res.Rings[i].Error == "" {
				res.Rings[i].Error = err.Error()
			}
			continue
		}
		for _, h := range ringHits {
			if h.Form == "" && len(rd.Forms()) == 1 {
				h.Form = rd.Forms()[0]
			}
			if !q.allowsForm(h.Form) {
				continue
			}
			if h.Ring == "" {
				h.Ring = rd.Ring()
			}
			h.Memory = q.Memory
			if h.Ring == RingAgent && h.Checkpoint && h.Relation == "" && (q.Episode == "" || q.Episode != h.Episode) {
				continue
			}
			byRing[rd.Ring()] = append(byRing[rd.Ring()], h)
		}
	}
	var hits []Hit
	for _, ring := range res.Rings {
		ringHits := byRing[ring.Name]
		sort.SliceStable(ringHits, func(i, j int) bool {
			if ringHits[i].Score != ringHits[j].Score {
				return ringHits[i].Score > ringHits[j].Score
			}
			return ringHits[i].ID < ringHits[j].ID
		})
		if len(ringHits) > q.k() {
			ringHits = ringHits[:q.k()]
		}
		hits = append(hits, ringHits...)
	}

	if q.MinCoverage > 0 && len(hits) == 0 {
		res.Abstained = true
		return res
	}
	res.Blocks, res.Budget.Used = budgetBlocks(hits, q.Budget)
	res.Blocks = workingLast(res.Blocks)
	return res
}

// workingLast recites the episode's working notes after every other block
// (recency, design §4). It runs AFTER selection and budgeting, so ring
// precedence, per-ring K and the budget decide WHAT is shown exactly as
// before; only the presentation order of the working blocks moves. Stable,
// so each group keeps its ring-precedence order.
func workingLast(blocks []ContextBlock) []ContextBlock {
	out := make([]ContextBlock, 0, len(blocks))
	for _, b := range blocks {
		if !b.working {
			out = append(out, b)
		}
	}
	for _, b := range blocks {
		if b.working {
			out = append(out, b)
		}
	}
	return out
}

func orderedReaders(q Query, readers []Reader) []Reader {
	wanted := q.Rings
	// After --memory resolution an empty Rings is an empty intersection
	// (match nothing), not the historical every-ring default — unless the
	// memory row left rings unconstrained (code).
	if len(wanted) == 0 && !q.memoryRingsStrict {
		wanted = Rings()
	}
	order := map[string]int{}
	for i, ring := range Rings() {
		order[ring] = i
	}
	next := len(order)
	for _, ring := range wanted {
		if _, ok := order[ring]; !ok {
			order[ring] = next
			next++
		}
	}
	var out []Reader
	for _, rd := range readers {
		if slices.Contains(wanted, rd.Ring()) {
			out = append(out, rd)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Ring()] < order[out[j].Ring()] })
	return out
}

func readerIntersectsForms(rd Reader, q Query) bool {
	if len(q.Forms) == 0 {
		return !q.memoryFormsStrict
	}
	for _, form := range q.Forms {
		if slices.Contains(rd.Forms(), form) {
			return true
		}
	}
	return false
}

func budgetBlocks(hits []Hit, limit int) ([]ContextBlock, int) {
	blocks := make([]ContextBlock, 0, len(hits))
	included := make([]Hit, 0, len(hits))
	used := 0
	for _, h := range hits {
		b := blockAt(h, "cue")
		if limit > 0 && used+b.Tokens > limit {
			continue
		}
		blocks = append(blocks, b)
		included = append(included, h)
		used += b.Tokens
	}
	for i, h := range included {
		resolutions := []string{"line"}
		if strings.TrimSpace(h.Full) != "" {
			resolutions = append([]string{"full"}, resolutions...)
		}
		for _, resolution := range resolutions {
			if resolution == "line" && strings.TrimSpace(h.Gist) == "" {
				continue
			}
			candidate := blockAt(h, resolution)
			if limit <= 0 || used-blocks[i].Tokens+candidate.Tokens <= limit {
				used += candidate.Tokens - blocks[i].Tokens
				blocks[i] = candidate
				break
			}
		}
	}
	return blocks, used
}

func blockAt(h Hit, resolution string) ContextBlock {
	text := strings.TrimSpace(h.Cue)
	if resolution != "cue" && strings.TrimSpace(h.Gist) != "" {
		text += " — " + strings.TrimSpace(h.Gist)
	}
	if resolution == "full" && strings.TrimSpace(h.Full) != "" {
		body := []rune(strings.Join(strings.Fields(h.Full), " "))
		if len(body) > kb.DefaultBodyCap {
			body = append(body[:kb.DefaultBodyCap], '…')
		}
		text += "\n" + string(body)
	}
	b := ContextBlock{
		Ring: h.Ring, Form: h.Form, Ref: h.ID, Memory: h.Memory,
		Resolution: resolution, Status: h.Status, Text: text,
		ObservedAt: h.ObservedAt, working: h.Working,
		Why: append([]string(nil), h.Why...), Source: append([]SourceRef(nil), h.Source...),
	}
	// ValidTo, Freshness, Snapshot and Path stay empty in M1: no source
	// record carries them yet (M6 owns valid_to; the sense schema owns
	// freshness/snapshot; M4 owns path). Declared here, never emitted.
	b.Tokens = estimateContextTokens(b)
	return b
}

// estimateContextTokens is the same deliberately cheap estimator used by the
// recall preamble: rendered words plus a fixed per-block envelope allowance.
func estimateContextTokens(b ContextBlock) int {
	return len(strings.Fields(b.Text)) + 8
}

func renderContext(w io.Writer, res ContextResult) {
	if res.Abstained {
		fmt.Fprintf(w, "nothing is known about %q above the coverage threshold\n", res.For)
		return
	}
	if len(res.Blocks) == 0 {
		fmt.Fprintf(w, "nothing is known about %q\n", res.For)
		return
	}
	for _, b := range res.Blocks {
		fmt.Fprintf(w, "- [%s/%s] %s  (%s)\n", b.Ring, b.Form, strings.ReplaceAll(b.Text, "\n", " "), b.Ref)
	}
}
