package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/kbmemory"
	"github.com/qiangli/yoke/pkg/repomap"
)

// Multi-hop recall: a query-conditioned graph walk over the visible memory.
//
// Hops > 0 expands today's per-ring hits along the graph the requesting
// principal can see — kb typed relations, [[links]] prose citations, and,
// only when the code form is requested, code-graph edges behind a CodeGraph
// reader. The walk is personalized PageRank from the query's seed hits over
// that visible graph (repomap.PageRank, never a copy), so a page that shares
// no query keywords still surfaces when the graph binds it to what matched.
//
// Visibility first: the graph is built ONLY from nodes and edges visible to
// the requesting principal — the same ring/scope/lineage/lifecycle filters
// recall already applies (visiblePages, RelationRing.live, kb.ScopeMatches),
// resolved BEFORE ranking. A hidden node never bridges, scores, appears in a
// path, shifts counts, or leaks into follow-ups: it is absent from the graph,
// not demoted in it. The walk runs on one graph PER RING, so structure never
// flows across rings either.
//
// Relations are edges, not nodes: a live link relation becomes a labelled
// edge between the record nodes its endpoints resolve to, in both directions
// (a citation and its backlink bind equally for recall). Only endpoints that
// resolve to a known record node in the same ring enter the walk — entity,
// todo and external refs do not, so every ref in an emitted path names a
// record the same principal can re-open. Relation records therefore bridge
// the walk but are never multi-hop targets; their labels ride the path.
//
// Scoring: final = 1/2 text + 1/2 structure, each max-normalized within its
// ring (scores from different rings stay incomparable — no fusion, per-ring
// K kept). Seeds keep today's ring scores as their text term; discovered
// pages are text-scored by the same kb ranker over the visible pool;
// discovered code carries no text term (its ranker lives behind CodeRing and
// is never re-implemented here). Structure is the quantized personalized
// PageRank mass. Ties break by ref ascending, so output is byte-identical
// across runs.
//
// Bounds: candidates sit at most --hops edges from their nearest seed; each
// node's distinct out-neighbours are capped (MaxHopFanout, highest weight
// first, ref tie-break); the graph itself is capped (MaxHopNodes, most
// query-relevant kept). Emission still flows through the per-ring K caps and
// the result token budget, so a high-degree hub cannot blow any ceiling.
const (
	// MaxHopFanout caps distinct out-neighbours per graph node, keeping a
	// hub's adjacency (and the walk's cost) bounded.
	MaxHopFanout = 32
	// MaxHopNodes caps nodes per ring graph; beyond it the most
	// query-relevant (pool text score, slug tie-break) are kept.
	MaxHopNodes = 2000
	// maxHopBodyBytes caps the prose scanned for [[links]] per page. Pages
	// are small; this is a guard rail, not a tuning knob.
	maxHopBodyBytes = 256 << 10
	// hopTextWeight and hopStructWeight are the documented final-score mix.
	hopTextWeight   = 0.5
	hopStructWeight = 0.5
)

// hopRelationWeights is the small table of edge weights by relation type.
// The walk runs on repomap's unweighted PageRank, so a weight becomes edge
// multiplicity: 1 + round(weight*2) parallel edges, i.e. about/ tested-by
// carry three votes per step, prose links two, supersedes two. Unknown
// (open-vocabulary) relations cite at prose strength.
var hopRelationWeights = map[string]float64{
	"about":        1.0,
	"tested-by":    1.0,
	"calls":        0.9,
	"depends-on":   0.8,
	"part-of":      0.8,
	"supports":     0.7,
	"derived-from": 0.7,
	"decided-in":   0.6,
	"observed":     0.6,
	"caused-by":    0.6,
	"contradicts":  0.5,
	"link":         0.5,
	"supersedes":   0.4,
}

// hopWeight returns the walk weight of a relation label.
func hopWeight(rel string) float64 {
	if w, ok := hopRelationWeights[strings.ToLower(strings.TrimSpace(rel))]; ok {
		return w
	}
	return 0.5
}

// hopMultiplicity converts a weight to parallel-edge count for the
// unweighted walk.
func hopMultiplicity(w float64) int { return 1 + int(math.Round(w*2)) }

// SeedSource supplies extra multi-hop seeds beside today's per-ring recall
// hits. The default (nil Query.Seeds) walks from the per-ring hits alone. A
// set source contributes additional seed hits; its hits join the emission
// pool of the ring they name, while a hit with no ring seeds the walk only.
type SeedSource func(q Query) ([]Hit, error)

// LabeledEdge is one directed graph edge between hit IDs.
type LabeledEdge struct {
	From, To string
	// Relation is the label the path shows for this step (about, calls,
	// link, ...), not a ref.
	Relation string
}

// VisibleGraph is implemented by readers that can enumerate the
// principal-visible graph behind them: candidate node hits plus the directed
// edges between endpoint refs. Readers without it contribute only their
// recall hits, as isolated seed nodes.
type VisibleGraph interface {
	Reader
	VisibleNodes(q Query) (nodes []Hit, edges []LabeledEdge, err error)
}

// CodeGraph is implemented by code-form readers that can enumerate their
// code graph: candidate code hits plus calls/tested-by/contains edges. Code
// nodes and edges enter the walk ONLY when the query requests the code form;
// otherwise the code reader contributes isolated seed nodes like any other
// reader without a graph. (cmds/graph's CodeRing may implement this to wire
// the live code graph; the walk never imports it.)
type CodeGraph interface {
	Reader
	CodeNodes(q Query) (nodes []Hit, edges []LabeledEdge, err error)
}

// MemorySeeds is the kbmemory adapter seed source: it ranks with the shared
// scoped lexical retrieval (Retrieve) instead of re-implementing ranking,
// and contributes the ranked records as walk seeds.
//
// Adapter contract: the recall query's --as-of travels in
// RetrieveQuery.AsOf (kbmemory resolves the same pkg/kblife visibility, one
// fixed now when empty); a served Hit.Ref is the stable record ID VERBATIM
// (the emitted recall Hit.ID is never rewritten); the canonical source
// target travels in the freshness report, so the graph node ref is built
// from (DocTarget, Hit.Ref) via MemoryNodeRef. Ring names the emission ring
// for these records; empty means walk seeds only (never emitted).
type MemorySeeds struct {
	Target, Index, Session, Dir string
	Mode                        string
	Ring                        string
	// Call runs retrieval; nil means kbmemory.Retrieve. Tests stub it.
	Call func(ctx context.Context, q kbmemory.RetrieveQuery) kbmemory.Envelope
}

// Source implements the SeedSource signature.
func (c MemorySeeds) Source(q Query) ([]Hit, error) {
	asOf := ""
	if !q.AsOf.IsZero() {
		asOf = q.asOf().UTC().Format(time.RFC3339Nano)
	}
	call := c.Call
	if call == nil {
		call = kbmemory.Retrieve
	}
	env := call(context.Background(), kbmemory.RetrieveQuery{
		Mode: c.Mode, Target: c.Target, Index: c.Index, Session: c.Session,
		Dir: c.Dir, Text: q.Text, Limit: q.k(), AsOf: asOf,
	})
	if env.Error != nil || !env.OK {
		msg := "memory.retrieve failed"
		if env.Error != nil && strings.TrimSpace(env.Error.Message) != "" {
			msg = env.Error.Message
		}
		return nil, fmt.Errorf("kb: memory seeds: %s", msg)
	}
	var res kbmemory.RetrieveResult
	switch r := env.Result.(type) {
	case kbmemory.RetrieveResult:
		res = r
	case *kbmemory.RetrieveResult:
		if r != nil {
			res = *r
		}
	default:
		raw, err := json.Marshal(env.Result)
		if err != nil {
			return nil, fmt.Errorf("kb: memory seeds: decode result: %v", err)
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("kb: memory seeds: decode result: %v", err)
		}
	}
	out := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		form := strings.TrimSpace(h.Record.Form)
		switch form {
		case kb.FormNote, kb.FormPage, kb.FormRelation, kb.FormCode:
		default:
			form = kb.FormPage
		}
		body := strings.TrimSpace(h.Record.Body)
		gist := body
		if len([]rune(gist)) > 120 {
			gist = string([]rune(gist)[:120])
		}
		out = append(out, Hit{
			Ring: c.Ring, Kind: "memory", ID: h.Ref, Form: form,
			Cue: strings.TrimSpace(h.Record.Title), Gist: gist, Full: body,
			Source: []SourceRef{{
				URI:    strings.TrimSpace(res.Freshness.DocTarget),
				Region: h.Ref,
			}},
			Status: strings.TrimSpace(h.Record.Status), Confidence: "RETRIEVED",
			ObservedAt: strings.TrimSpace(h.Record.Updated),
			ValidFrom:  strings.TrimSpace(h.Record.ValidFrom),
			ValidTo:    strings.TrimSpace(h.Record.ValidTo),
			Score:      h.Score,
			Why:        append(append([]string{"memory"}, h.Why...), "lexical"),
		})
	}
	return out, nil
}

// MemoryNodeRef builds the graph node ref for a memory record from its
// canonical source target and its verbatim record ID. The emitted Hit.ID
// stays the bare record ID; only the graph key carries the target, so two
// collections holding the same ID never collide.
func MemoryNodeRef(docTarget, ref string) string {
	docTarget = strings.TrimSpace(docTarget)
	ref = strings.TrimSpace(ref)
	if docTarget == "" {
		return "memory:" + ref
	}
	return "memory:" + docTarget + "#" + ref
}

// memoryNodeOf returns the graph node key for an adapter seed hit: the
// (DocTarget, Ref) ref when the hit carries its source target, else the bare
// ID. The second return reports whether the key is target-qualified.
func memoryNodeOf(h Hit) (string, bool) {
	for _, s := range h.Source {
		if strings.TrimSpace(s.Region) == h.ID && strings.TrimSpace(s.URI) != "" {
			return MemoryNodeRef(s.URI, h.ID), true
		}
	}
	return h.ID, false
}

// normalizeHopEndpoint maps a relation endpoint or link target to a node
// ref: a kind:id ref canonicalizes, a bare token is a kb slug. Empty and
// malformed endpoints resolve to nothing and their edge is dropped.
func normalizeHopEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, ":"); i > 0 {
		kind, id := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		if kind == "" || id == "" {
			return ""
		}
		return strings.ToLower(kind) + ":" + id
	}
	return "kb:" + s
}

// pageGraphNodes enumerates one page ring's visible graph: candidate page
// hits (text-scored by the same kb ranker over the visible pool, forms and
// types unrestrained — those select emission, not visibility) plus [[links]]
// edges. Lifecycle, scope, lineage, age and checkpoint gates are the same
// ones ranking applies, so hidden pages are absent before ranking.
func pageGraphNodes(ring string, store *kb.Store, path string, q Query) ([]Hit, []LabeledEdge, error) {
	gq := q
	gq.Forms, gq.Types = nil, nil
	gq.memoryRingsStrict, gq.memoryFormsStrict = false, false
	pages, relation, err := visiblePages(ring, store, path, gq)
	if err != nil {
		return nil, nil, err
	}
	pages = slices.DeleteFunc(pages, func(p *kb.Page) bool {
		return !kb.ScopeMatches(p, q.Repo, q.OS)
	})
	scores := map[string]float64{}
	if len(pages) > 0 {
		for _, h := range kb.Search(pages, kb.Query{
			Terms: kb.Terms(q.Text), Repo: q.Repo, OS: q.OS,
			K: max(len(pages), 1), All: true,
		}) {
			scores[h.Page.Slug] = h.Score
		}
	}
	sort.Slice(pages, func(i, j int) bool {
		if scores[pages[i].Slug] != scores[pages[j].Slug] {
			return scores[pages[i].Slug] > scores[pages[j].Slug]
		}
		return pages[i].Slug < pages[j].Slug
	})
	if len(pages) > MaxHopNodes {
		pages = pages[:MaxHopNodes]
	}
	keep := map[string]bool{}
	for _, p := range pages {
		keep[p.Slug] = true
	}
	nodes := make([]Hit, 0, len(pages))
	var edges []LabeledEdge
	for _, p := range pages {
		hit, ok := pageHit(ring, store, q, relation, p, scores[p.Slug], 0)
		if !ok {
			delete(keep, p.Slug)
			continue
		}
		nodes = append(nodes, hit)
		if len(p.Body) > maxHopBodyBytes {
			continue
		}
		for _, l := range kb.ParseLinks(p.Body) {
			if l.Kind != kb.LinkKB {
				continue
			}
			to := "kb:" + l.Target
			if to == hit.ID || !keep[l.Target] {
				continue
			}
			edges = append(edges, LabeledEdge{From: hit.ID, To: to, Relation: kb.BodyLinkRelation})
		}
	}
	return nodes, edges, nil
}

func (r RepoRing) VisibleNodes(q Query) ([]Hit, []LabeledEdge, error) {
	return pageGraphNodes(RingRepo, r.Store, r.Path, q)
}

func (r HostRing) VisibleNodes(q Query) ([]Hit, []LabeledEdge, error) {
	return pageGraphNodes(RingHost, r.Store, r.Path, q)
}

func (r AgentRing) VisibleNodes(q Query) ([]Hit, []LabeledEdge, error) {
	return pageGraphNodes(RingAgent, r.Store, r.Path, q)
}

// VisibleNodes exposes the ring's live link relations as labelled edges.
// There are no relation nodes: relations bridge the walk between the record
// nodes their endpoints resolve to. Endpoint resolution (same-ring record
// check) happens at assembly; here every well-formed link relation becomes
// a candidate edge.
func (r RelationRing) VisibleNodes(q Query) ([]Hit, []LabeledEdge, error) {
	live, err := r.live()
	if err != nil {
		return nil, nil, err
	}
	var edges []LabeledEdge
	for _, rel := range live {
		if rel.Op != "link" {
			continue
		}
		from, to := normalizeHopEndpoint(rel.Target), normalizeHopEndpoint(rel.Dst)
		if from == "" || to == "" || from == to {
			continue
		}
		label := strings.TrimSpace(rel.Relation)
		if label == "" {
			label = kb.BodyLinkRelation
		}
		edges = append(edges, LabeledEdge{From: from, To: to, Relation: label})
	}
	return nil, edges, nil
}

// hopNode is one assembled graph node.
type hopNode struct {
	hit  Hit
	text float64
	seed bool
}

// hopPrev is one BFS step: where the walk came from and over which label.
type hopPrev struct {
	from  string
	label string
}

// expandSeeds merges multi-hop discoveries into the per-ring seed hits. It
// returns the merged hits in ring order (per-ring K kept, no fusion) and any
// ring-scoped expansion errors for the caller to report beside its own read
// errors. Hops <= 0 returns the seeds untouched.
func expandSeeds(q Query, readers []Reader, seeds []Hit) ([]Hit, map[string]error) {
	errs := map[string]error{}
	if q.Hops <= 0 {
		return seeds, errs
	}
	if q.Seeds != nil {
		extra, err := q.Seeds(q)
		if err != nil {
			errs["memory"] = err
		} else {
			seeds = append(seeds, extra...)
		}
	}
	// Dedupe by ring+ID, first wins (today's hits before adapter seeds).
	seen := map[string]bool{}
	deduped := seeds[:0]
	for _, h := range seeds {
		key := h.Ring + "\x00" + h.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, h)
	}
	seeds = deduped

	var order []string
	byRing := map[string][]Hit{}
	for _, h := range seeds {
		if _, ok := byRing[h.Ring]; !ok {
			order = append(order, h.Ring)
		}
		byRing[h.Ring] = append(byRing[h.Ring], h)
	}

	// Assemble one graph per ring.
	nodes := map[string]map[string]*hopNode{}
	rawEdges := map[string][]LabeledEdge{}
	claim := func(ring, id string, hit Hit, text float64) {
		m := nodes[ring]
		if m == nil {
			m = map[string]*hopNode{}
			nodes[ring] = m
		}
		if _, ok := m[id]; !ok {
			m[id] = &hopNode{hit: hit, text: text}
		}
	}
	for _, rd := range readers {
		ring := rd.Ring()
		if vg, ok := rd.(VisibleGraph); ok {
			nh, eh, err := vg.VisibleNodes(q)
			if err != nil {
				errs[ring] = err
				continue
			}
			for _, h := range nh {
				claim(ring, h.ID, h, h.Score)
			}
			rawEdges[ring] = append(rawEdges[ring], eh...)
		}
		if cg, ok := rd.(CodeGraph); ok {
			if !q.allowsForm(kb.FormCode) {
				continue
			}
			nh, eh, err := cg.CodeNodes(q)
			if err != nil {
				errs[ring] = err
				continue
			}
			for _, h := range nh {
				if strings.TrimSpace(h.ID) == "" {
					continue
				}
				claim(ring, h.ID, h, 0)
			}
			rawEdges[ring] = append(rawEdges[ring], eh...)
		}
	}
	// Every seed is a node, isolated when the graph never mentions it. A
	// qualified adapter seed with a ring lives under its (DocTarget, Ref)
	// node ref (MemoryNodeRef), so two collections holding the same record
	// ID never collide; every other seed lives under its bare hit ID, so a
	// walk-only seed still bridges when the graph names it. The first payload
	// wins on coincidence (ring record nodes are claimed before seeds), with
	// the seed's text term taking the max — a coincident lexical seed boosts
	// the record instead of replacing it. Emission dedupes by hit ID, so a
	// coincident pair can never emit twice.
	for ring, hs := range byRing {
		if ring == "" {
			continue
		}
		for _, h := range hs {
			nodeID := h.ID
			if key, qualified := memoryNodeOf(h); qualified {
				nodeID = key
			}
			if n, ok := nodes[ring][nodeID]; ok {
				n.seed = true
				if h.Score > n.text {
					n.text = h.Score
				}
			} else {
				claim(ring, nodeID, h, h.Score)
				nodes[ring][nodeID].seed = true
			}
		}
	}

	// Walk-only (ring-less) seeds with a bare ID attach to every ring whose
	// graph already names that record: they boost and seed the record they
	// name without emitting anywhere. Target-qualified walk-only seeds stay
	// isolated — nothing in the kb graph can name a (target, ref) key.
	for _, h := range byRing[""] {
		if _, qualified := memoryNodeOf(h); qualified {
			continue
		}
		for _, m := range nodes {
			if n, ok := m[h.ID]; ok {
				n.seed = true
				if h.Score > n.text {
					n.text = h.Score
				}
			}
		}
	}

	var out []Hit
	for _, ring := range order {
		if ring == "" {
			continue // walk-only seeds never emit
		}
		out = append(out, expandRing(q, nodes[ring], rawEdges[ring], byRing[ring])...)
	}
	return out, errs
}

// seedNodeIDs returns the graph node keys a seed hit answers to: its bare
// hit ID, plus its (DocTarget, Ref) key when it carries one.
func seedNodeIDs(h Hit) []string {
	ids := []string{h.ID}
	if key, qualified := memoryNodeOf(h); qualified && key != h.ID {
		ids = append(ids, key)
	}
	return ids
}

// expandRing runs the per-ring walk: BFS-bounded candidates, personalized
// PageRank over the ring graph, final-score merge under per-ring K.
func expandRing(q Query, ns map[string]*hopNode, edges []LabeledEdge, seeds []Hit) []Hit {
	if len(seeds) == 0 {
		return nil
	}
	// Distinct out-neighbours per node, highest weight first, ref tie-break,
	// capped at MaxHopFanout — then expanded by multiplicity.
	type weightedTo struct {
		to     string
		weight float64
		label  string
	}
	byFrom := map[string]map[string]weightedTo{}
	add := func(from, to, label string) {
		if from == to {
			return
		}
		m := byFrom[from]
		if m == nil {
			m = map[string]weightedTo{}
			byFrom[from] = m
		}
		w := hopWeight(label)
		if cur, ok := m[to]; !ok || w > cur.weight || (w == cur.weight && label < cur.label) {
			m[to] = weightedTo{to: to, weight: w, label: label}
		}
	}
	for _, e := range edges {
		from, to := strings.TrimSpace(e.From), strings.TrimSpace(e.To)
		if from == "" || to == "" || from == to {
			continue
		}
		if _, ok := ns[from]; !ok {
			continue
		}
		if _, ok := ns[to]; !ok {
			continue
		}
		label := strings.TrimSpace(e.Relation)
		if label == "" {
			label = kb.BodyLinkRelation
		}
		add(from, to, label)
		add(to, from, label)
	}
	adj := map[string][]string{}
	labels := map[string]string{}
	for from, m := range byFrom {
		all := make([]weightedTo, 0, len(m))
		for _, wt := range m {
			all = append(all, wt)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].weight != all[j].weight {
				return all[i].weight > all[j].weight
			}
			return all[i].to < all[j].to
		})
		if len(all) > MaxHopFanout {
			all = all[:MaxHopFanout]
		}
		for _, wt := range all {
			for i := 0; i < hopMultiplicity(wt.weight); i++ {
				adj[from] = append(adj[from], wt.to)
			}
			labels[from+"\x00"+wt.to] = wt.label
		}
	}
	for id := range ns {
		if _, ok := adj[id]; !ok {
			adj[id] = nil
		}
	}
	// Multi-source BFS from the seeds (ref order), bounded by Hops. Seed
	// flags come from assembly (per-ring seeds plus attached walk-only
	// seeds); the seeds argument re-asserts its own node keys.
	seedSet := map[string]bool{}
	for id, n := range ns {
		if n.seed {
			seedSet[id] = true
		}
	}
	for _, h := range seeds {
		for _, id := range seedNodeIDs(h) {
			if _, ok := ns[id]; ok {
				seedSet[id] = true
			}
		}
	}
	seedIDs := make([]string, 0, len(seedSet))
	for id := range seedSet {
		seedIDs = append(seedIDs, id)
	}
	sort.Strings(seedIDs)
	dist := map[string]int{}
	prevs := map[string]hopPrev{}
	queue := make([]string, 0, len(seedIDs))
	for _, id := range seedIDs {
		if _, seen := dist[id]; seen {
			continue
		}
		if _, ok := ns[id]; !ok {
			continue
		}
		dist[id] = 0
		queue = append(queue, id)
	}
	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if dist[cur] >= q.Hops {
			continue
		}
		seen := map[string]bool{}
		var next []string
		for _, nb := range adj[cur] {
			if !seen[nb] {
				seen[nb] = true
				next = append(next, nb)
			}
		}
		sort.Strings(next)
		for _, nb := range next {
			if _, ok := dist[nb]; ok {
				continue
			}
			dist[nb] = dist[cur] + 1
			prevs[nb] = hopPrev{from: cur, label: labels[cur+"\x00"+nb]}
			queue = append(queue, nb)
		}
	}
	// Personalized PageRank from the seeds' text mass. Each seed credits
	// its full share to every node key it answers to (bare ID and, for
	// adapter seeds, the (DocTarget, Ref) key): the vector is a teleport
	// bias, not a distribution, so coincident views share the mass rather
	// than splitting it.
	pers := map[string]float64{}
	sum := 0.0
	for _, h := range seeds {
		sum += clampScore(h.Score)
	}
	if sum <= 0 {
		for _, id := range seedIDs {
			pers[id] += 1.0 / float64(max(len(seedIDs), 1))
		}
	} else {
		for _, h := range seeds {
			for _, id := range seedNodeIDs(h) {
				if _, ok := ns[id]; ok {
					pers[id] += clampScore(h.Score) / sum
				}
			}
		}
	}
	structure := repomap.PageRank(adj, pers, 20, 0.85)
	// Final = 1/2 text + 1/2 structure, max-normalized in-ring, quantized
	// so float summation order (map iteration) cannot break byte-identity.
	maxText, maxStruct := 0.0, 0.0
	for id, n := range ns {
		if n.text > maxText {
			maxText = n.text
		}
		if s := quantize(structure[id]); s > maxStruct {
			maxStruct = s
		}
	}
	final := map[string]float64{}
	for id, n := range ns {
		t, s := 0.0, quantize(structure[id])
		if maxText > 0 {
			t = n.text / maxText
		}
		if maxStruct > 0 {
			s /= maxStruct
		} else {
			s = 0
		}
		final[id] = quantize(hopTextWeight*t + hopStructWeight*s)
	}
	// Emit seeds plus in-bound discoveries: form-selected record nodes only.
	// Relation records bridge (their labels ride paths) but are never
	// targets; code needs its form requested (gated at assembly).
	type hopCand struct {
		id string // graph node key (a memory view's key differs from its hit ID)
		n  *hopNode
	}
	pool := make([]hopCand, 0, len(ns))
	for id, n := range ns {
		if seedSet[id] {
			pool = append(pool, hopCand{id: id, n: n})
			continue
		}
		d, ok := dist[id]
		if !ok || d == 0 || d > q.Hops {
			continue
		}
		if !q.allowsForm(n.hit.Form) {
			continue
		}
		if strings.TrimSpace(n.hit.Form) == kb.FormRelation {
			continue
		}
		pool = append(pool, hopCand{id: id, n: n})
	}
	sort.Slice(pool, func(i, j int) bool {
		fi, fj := final[pool[i].id], final[pool[j].id]
		if fi != fj {
			return fi > fj
		}
		return pool[i].n.hit.ID < pool[j].n.hit.ID
	})
	// One emission per record: a coincident pair (record node plus adapter
	// view) keeps the higher final, deterministically.
	uniq := pool[:0]
	seenIDs := map[string]bool{}
	for _, c := range pool {
		if seenIDs[c.n.hit.ID] {
			continue
		}
		seenIDs[c.n.hit.ID] = true
		uniq = append(uniq, c)
	}
	pool = uniq
	if len(pool) > q.k() {
		pool = pool[:q.k()]
	}
	out := make([]Hit, 0, len(pool))
	for _, c := range pool {
		h := c.n.hit
		h.Score = final[c.id]
		h.Memory = q.Memory
		if !seedSet[c.id] {
			// Adapter view keys never reach here (always seeds), so the
			// path's refs are record node IDs verbatim.
			h.Path = hopPath(c.id, prevs)
			h.Why = append([]string{"multihop"}, "via "+strings.Join(hopLabels(h.Path), "+"))
		}
		out = append(out, h)
	}
	return out
}

// hopPath rebuilds the seed-to-node path: alternating node refs and the
// relation label joining each step, ending at the hit's own ref.
func hopPath(id string, prevs map[string]hopPrev) []string {
	var labels []string
	var refs []string
	for cur := id; ; {
		refs = append(refs, cur)
		p, ok := prevs[cur]
		if !ok {
			break
		}
		labels = append(labels, p.label)
		cur = p.from
	}
	slices.Reverse(refs)
	slices.Reverse(labels)
	path := make([]string, 0, 2*len(refs)-1)
	for i, r := range refs {
		if i > 0 {
			label := kb.BodyLinkRelation
			if labels[i-1] != "" {
				label = labels[i-1]
			}
			path = append(path, label)
		}
		path = append(path, r)
	}
	return path
}

// hopLabels returns the relation labels carried by a path.
func hopLabels(path []string) []string {
	var out []string
	for i := 1; i+1 < len(path); i += 2 {
		out = append(out, path[i])
	}
	if len(out) == 0 {
		out = []string{kb.BodyLinkRelation}
	}
	return out
}

// clampScore keeps seed text mass finite and non-negative.
func clampScore(x float64) float64 {
	if x < 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

// quantize rounds to 1e-6 so map-order float noise cannot surface in output.
func quantize(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x*1e6) / 1e6
}
