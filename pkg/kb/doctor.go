package kb

import (
	"os"
	"sort"
	"strings"
)

// doctor FLAGS, never FIXES. It reads the resolved ring (plus the sibling
// docs/todo for the repo ring) and reports link-graph and hygiene problems — a
// dangling link, an orphan page, a near-duplicate pair, a record missing its
// form or description. It is deliberately NOT `kb fix`: nothing on this surface
// rewrites a body, so a reported body is left byte-identical on disk
// (TestDoctorLeavesBodyByteIdentical pins exactly that). Repair is a human or a
// model decision, made with `kb update`/`kb supersede`.

// DanglingLink is one body link whose target resolves to no record.
type DanglingLink struct {
	From   string `json:"from"`   // ref of the record that carries the link
	Target string `json:"target"` // the ref the link points at
	Raw    string `json:"raw"`    // the literal link text
}

// DupPair is two live pages NearDuplicate considers the same knowledge.
type DupPair struct {
	A string `json:"a"`
	B string `json:"b"`
}

type OpenRelation struct {
	ID       string `json:"id"`
	Target   string `json:"target,omitempty"`
	Relation string `json:"relation"`
	Dst      string `json:"dst,omitempty"`
}

// DoctorReport is the result of a doctor pass. Empty slices (not nil) so the
// JSON shape is stable whether or not a category fired.
type DoctorReport struct {
	Ring     string         `json:"ring,omitempty"`
	Dangling []DanglingLink `json:"dangling"`
	// UnknownKind is a body link whose scheme is not in the ref vocabulary
	// ([[note: x]], [[urn:dhnt:issue:1]]). Before the vocabulary existed these
	// were misfiled as dangling kb slugs; they are their own class now so the
	// row MOVES rather than vanishes when the parser stops guessing.
	UnknownKind        []DanglingLink `json:"unknown_kind"`
	Orphans            []string       `json:"orphans"`
	NearDuplicates     []DupPair      `json:"near_duplicates"`
	MissingForm        []string       `json:"missing_form"`
	MissingDescription []string       `json:"missing_description"`
	OpenRelations      []OpenRelation `json:"open_relations"`
	// MissingID lists pages that predate the id/seq handles (no `id:` or no
	// `seq:` in frontmatter) — they resolve by slug only until next written;
	// the repair is an explicit `kb update <slug>`, never a doctor rewrite.
	// DuplicateSeq lists running numbers two live pages share (two branches
	// each minted MaxSeq+1): `kb:<seq>` is ambiguous for them while slug and
	// uuid keep resolving; nothing is renumbered.
	MissingID    []string `json:"missing_id"`
	DuplicateSeq []DupSeq `json:"duplicate_seq"`
}

// DupSeq is one running number shared by more than one page in a ring.
type DupSeq struct {
	Seq   int      `json:"seq"`
	Slugs []string `json:"slugs"`
}

// Clean reports whether the pass found nothing.
func (r DoctorReport) Clean() bool {
	return len(r.Dangling) == 0 && len(r.UnknownKind) == 0 && len(r.Orphans) == 0 && len(r.NearDuplicates) == 0 &&
		len(r.MissingForm) == 0 && len(r.MissingDescription) == 0 &&
		len(r.OpenRelations) == 0 && len(r.MissingID) == 0 && len(r.DuplicateSeq) == 0
}

// Doctor runs every hygiene check over the kb pages, using todoNodes (when the
// caller could enumerate them — the repo ring's docs/todo) to resolve todo
// links and count todo→kb citations. todoKnown says whether todo links are
// enumerable: when false, a todo link is never flagged dangling (kb could not
// see the store, so it cannot know the link is broken). store, when non-nil,
// lets the form check read the raw frontmatter (ParsePage defaults a missing
// form to "page" in memory, so the only way to see a legacy record is the
// bytes — which doctor only READS).
func Doctor(pages []*Page, store *Store, todoNodes []LinkNode, todoKnown bool) DoctorReport {
	r := DoctorReport{
		Dangling:           []DanglingLink{},
		UnknownKind:        []DanglingLink{},
		Orphans:            []string{},
		NearDuplicates:     []DupPair{},
		MissingForm:        []string{},
		MissingID:          []string{},
		DuplicateSeq:       []DupSeq{},
		MissingDescription: []string{},
		OpenRelations:      []OpenRelation{},
	}
	kbNodes := KBNodes(pages)
	allNodes := append(append([]LinkNode{}, kbNodes...), todoNodes...)

	// Dangling and unknown-kind links across every record (kb and todo).
	for _, n := range allNodes {
		for _, l := range ParseLinks(n.Body) {
			switch {
			case l.Kind == LinkUnknown:
				r.UnknownKind = append(r.UnknownKind, DanglingLink{From: n.Ref(), Target: l.Target, Raw: l.Raw})
			case danglingLink(l, allNodes, todoKnown):
				r.Dangling = append(r.Dangling, DanglingLink{From: n.Ref(), Target: l.Ref(), Raw: l.Raw})
			}
		}
	}

	// Orphans, missing form, missing description — kb pages only. Superseded
	// pages are reachable through their successor and are never orphans.
	for _, p := range pages {
		if p.Status == StatusSuperseded {
			continue
		}
		node := KBNode(p)
		outbound := len(ParseLinks(p.Body)) > 0
		inbound := len(Backlinks(node, allNodes)) > 0
		if !outbound && !inbound && p.Status != StatusValidated {
			r.Orphans = append(r.Orphans, p.Slug)
		}
		// A note is the memo shape: description optional by design (page.go
		// FormNote), so its absence is not a hygiene problem.
		if strings.TrimSpace(p.Description) == "" && p.EffForm() != FormNote {
			r.MissingDescription = append(r.MissingDescription, p.Slug)
		}
		if store != nil && !declaresForm(store, p.Slug) {
			r.MissingForm = append(r.MissingForm, p.Slug)
		}
		if p.ID == "" || p.Seq == 0 {
			r.MissingID = append(r.MissingID, p.Slug)
		}
	}
	// Duplicate running numbers — a merge of two branches that each minted
	// MaxSeq+1. Reported, never renumbered: a renumber would silently move
	// what `kb:<seq>` opens.
	bySeq := map[int][]string{}
	for _, p := range pages {
		if p.Seq != 0 {
			bySeq[p.Seq] = append(bySeq[p.Seq], p.Slug)
		}
	}
	for seq, slugs := range bySeq {
		if len(slugs) > 1 {
			sort.Strings(slugs)
			r.DuplicateSeq = append(r.DuplicateSeq, DupSeq{Seq: seq, Slugs: slugs})
		}
	}
	sort.Slice(r.DuplicateSeq, func(i, j int) bool { return r.DuplicateSeq[i].Seq < r.DuplicateSeq[j].Seq })

	// Near-duplicate pairs — reuse NearDuplicate, the one reconciler.
	r.NearDuplicates = nearDuplicatePairs(pages)

	return r
}

func DoctorRelations(relations []Relation) []OpenRelation {
	var out []OpenRelation
	for _, rel := range relations {
		if rel.Op != "link" || strings.TrimSpace(rel.Relation) == "" || CoreRelation(rel.Relation) {
			continue
		}
		out = append(out, OpenRelation{ID: rel.ID, Target: rel.Target, Relation: rel.Relation, Dst: rel.Dst})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Relation != out[j].Relation {
			return out[i].Relation < out[j].Relation
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// danglingLink reports whether link l resolves to nothing within a namespace we
// can authoritatively enumerate. kb is always enumerable; todo only when
// todoKnown; every other vocabulary kind never (kb holds no sprint/run/meet/…
// records, so such a link is external, not broken — its own store answers).
func danglingLink(l Link, nodes []LinkNode, todoKnown bool) bool {
	switch l.Kind {
	case LinkKB:
		_, ok := ResolveLink(l, nodes)
		return !ok
	case LinkTodo:
		if !todoKnown {
			return false
		}
		_, ok := ResolveLink(l, nodes)
		return !ok
	}
	return false
}

// nearDuplicatePairs reports each unordered pair of live pages NearDuplicate
// links, deduplicated.
func nearDuplicatePairs(pages []*Page) []DupPair {
	var live []*Page
	for _, p := range pages {
		if p.Status != StatusSuperseded {
			live = append(live, p)
		}
	}
	seen := map[string]bool{}
	out := []DupPair{}
	for i, p := range live {
		others := make([]*Page, 0, len(live)-1)
		others = append(others, live[:i]...)
		others = append(others, live[i+1:]...)
		dup := NearDuplicate(others, p.Title, p.Description)
		if dup == nil {
			continue
		}
		a, b := p.Slug, dup.Slug
		if a > b {
			a, b = b, a
		}
		key := a + "\x00" + b
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, DupPair{A: a, B: b})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

// declaresForm reports whether the page file declares a form: key. A legacy
// record that predates the form facet has none; ParsePage fills it to "page" in
// memory, so the bytes are the only witness. doctor only READS them.
func declaresForm(store *Store, slug string) bool {
	b, err := os.ReadFile(store.PagePath(slug))
	if err != nil {
		return true // cannot read the bytes → do not cry wolf
	}
	fm, _, ok := splitFrontmatter(b)
	if !ok {
		return true
	}
	for line := range strings.SplitSeq(fm, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "form:") {
			return true
		}
	}
	return false
}
