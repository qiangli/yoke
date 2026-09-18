package kb

import (
	"fmt"
	"strings"
)

// Resolution is HOW MUCH of a page a caller materialises. It is the second
// axis of retrieval, orthogonal to ranking: which pages come back is the
// ranker's job, how much of each one arrives is this.
//
// It exists because the payload, not the ranker, is where the tokens go.
// Measured on a real 29-page store, one query, the same three results:
// the cue lines are 288 bytes and the routing lines are 1072 — 3.7×. Earlier
// comparisons between rankers in the eval harness were partly measuring this
// difference rather than the rankers (dhnt/docs/memory-eval-plan.md §4f-bis),
// which is the whole reason it is now explicit instead of hard-coded in seven
// format strings.
type Resolution int

const (
	// ResCue is the address only: slug + title. Enough to decide whether to
	// open a page, not enough to decide whether it APPLIES.
	ResCue Resolution = iota
	// ResLine adds status/type and the description — the routing surface
	// ("what + WHEN this applies"). The default, and deliberately so: the
	// description is long BECAUSE it carries the WHEN keywords, so capping it
	// would cut exactly the signal a caller needs to decide.
	ResLine
	// ResFull adds the body, capped.
	ResFull
)

// DefaultBodyCap bounds a body rendered at ResFull. Bodies are unbounded on
// disk; a caller injecting one into a prompt is not.
const DefaultBodyCap = 400

// Renderer formats pages at one resolution. Bullet and Sep exist so the
// existing call sites keep their exact output — the CLI index line, the
// indented sub-list, and the "- " bullet used by the prompt injectors are the
// same content at the same resolution, differing only in list punctuation.
type Renderer struct {
	Resolution Resolution
	Bullet     string // "" (CLI), "  " (indented sub-list), "- " (prompt bullet)
	Sep        string // between slug and the rest: "  " (CLI) or " " (bullets)
	BodyCap    int    // 0 = DefaultBodyCap; only consulted at ResFull
	// Ref prints the address as the canonical ref (`kb:<slug>`) instead of the
	// bare slug. `kb list` sets it so a row can be copied into prose as-is; the
	// injection and envelope renderers leave it off and stay byte-identical.
	Ref bool
}

// addr is the page's address as this renderer prints it. With Ref on, the
// running number rides in front (`#22 kb:<slug>`) — the handle a human types,
// shown beside the ref it stands for; the ref itself never carries the seq.
func (rd Renderer) addr(p *Page) string {
	if rd.Ref {
		return seqTag(p) + "kb:" + p.Slug
	}
	return p.Slug
}

// CueRenderer is the lean rendering: addresses only.
func CueRenderer() Renderer { return Renderer{Resolution: ResCue, Sep: "  "} }

// LineRenderer is the default rendering: the routing surface.
func LineRenderer() Renderer { return Renderer{Resolution: ResLine, Sep: "  "} }

// Page renders one page, newline-terminated.
func (rd Renderer) Page(p *Page) string {
	sep := rd.Sep
	if sep == "" {
		sep = " "
	}
	var b strings.Builder
	switch rd.Resolution {
	case ResCue:
		fmt.Fprintf(&b, "%s%s%s%s\n", rd.Bullet, rd.addr(p), sep, p.Title)
	default:
		fmt.Fprintf(&b, "%s%s%s[%s/%s] %s — %s\n",
			rd.Bullet, rd.addr(p), sep, p.Status, p.Type, p.Title, p.Description)
		if rd.Resolution == ResFull {
			if body := strings.TrimSpace(p.Body); body != "" {
				cap := rd.BodyCap
				if cap <= 0 {
					cap = DefaultBodyCap
				}
				fmt.Fprintf(&b, "  %s\n", truncateRunes(strings.ReplaceAll(body, "\n", " "), cap))
			}
		}
	}
	return b.String()
}

// Hits renders a result set.
func (rd Renderer) Hits(hits []Hit) string {
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(rd.Page(h.Page))
	}
	return b.String()
}

// truncateRunes cuts on a rune boundary and marks the cut, so a reader can
// tell a short body from a clipped one.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
