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
	Ring       string      `json:"ring"`
	Form       string      `json:"form"`
	Ref        string      `json:"ref"`
	Tokens     int         `json:"tokens"`
	Resolution string      `json:"resolution,omitempty"`
	Status     string      `json:"status,omitempty"`
	Text       string      `json:"text"`
	Why        []string    `json:"why,omitempty"`
	Source     []SourceRef `json:"source,omitempty"`
}

// Context ranks within each reader, preserves ring boundaries, then spends one
// global budget on presentation. It first reserves the cheapest cue for every
// hit that fits; only then does it upgrade each block to the largest resolution
// that fits. A high-ranked full page therefore cannot starve every later ring.
func Context(q Query, readers ...Reader) ContextResult {
	res := ContextResult{
		ContextVersion: ContextVersion,
		For:            q.Text,
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
		if !readerIntersectsForms(rd, q.Forms) {
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
			if len(q.Forms) > 0 && !slices.Contains(q.Forms, h.Form) {
				continue
			}
			if h.Ring == "" {
				h.Ring = rd.Ring()
			}
			if h.Ring == RingAgent && h.Checkpoint && (q.Episode == "" || q.Episode != h.Episode) {
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
	return res
}

func orderedReaders(q Query, readers []Reader) []Reader {
	wanted := q.Rings
	if len(wanted) == 0 {
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

func readerIntersectsForms(rd Reader, forms []string) bool {
	if len(forms) == 0 {
		return true
	}
	for _, form := range forms {
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
		Ring: h.Ring, Form: h.Form, Ref: h.ID,
		Resolution: resolution, Status: h.Status, Text: text,
		Why: append([]string(nil), h.Why...), Source: append([]SourceRef(nil), h.Source...),
	}
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
