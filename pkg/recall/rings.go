package recall

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
)

// RepoRing reads the current repository's committed docs/kb store.
type RepoRing struct {
	Store *kb.Store
	Path  string
}

func (RepoRing) Ring() string    { return RingRepo }
func (RepoRing) Forms() []string { return []string{kb.FormNote, kb.FormPage} }
func (r RepoRing) Recall(q Query) ([]Hit, error) {
	return recallPages(RingRepo, r.Store, r.Path, q)
}

type RelationRing struct {
	RingName string
	Path     string
	// Owner scopes the AGENT relation ring to one principal, mirroring the agent
	// page ring (kb.OpenAgentRing). The agent relation store — the Sense-derived
	// "observed" and "decided-in" prefixes — is physically shared across
	// principals exactly like the agent page store, so a relation authored by
	// another principal must be invisible and a missing principal (empty Owner)
	// must see nothing (Q8). Owner is empty for the repo and host rings, whose
	// relation stores are shared by design.
	Owner string
}

func (r RelationRing) Ring() string { return r.RingName }
func (RelationRing) Forms() []string {
	return []string{kb.FormRelation}
}
func (r RelationRing) Recall(q Query) ([]Hit, error) {
	live, err := r.live()
	if err != nil {
		return nil, err
	}
	relations := kb.SearchRelations(live, kb.Terms(q.Text), q.k())
	out := make([]Hit, 0, len(relations))
	for _, rel := range relations {
		out = append(out, Hit{
			Ring: r.RingName, Kind: "relation", ID: "graph:" + rel.ID, Form: kb.FormRelation,
			Cue: kb.RelationText(rel), Confidence: rel.Confidence,
			ObservedAt: rel.At.Format(time.RFC3339),
			Score:      1,
			Why:        []string{"relation"},
			Source:     []SourceRef{{URI: "file://" + kb.RelationPath(r.Path)}},
			Episode:    rel.Episode,
		})
	}
	return out, nil
}

// AgentRing reads the owner-scoped <agent-data>/kb store. The supplied Store
// must be opened with kb.OpenAgentRing so another principal is invisible.
type AgentRing struct {
	Store    *kb.Store
	Path     string
	Required bool
}

func (AgentRing) Ring() string    { return RingAgent }
func (AgentRing) Forms() []string { return []string{kb.FormNote, kb.FormPage} }
func (r AgentRing) Recall(q Query) ([]Hit, error) {
	if r.Required && strings.TrimSpace(r.Path) == "" {
		return nil, fmt.Errorf("kb: open <agent-data>/kb: YCODE_DATA_DIR is not set")
	}
	return recallPages(RingAgent, r.Store, r.Path, q)
}

// live returns the relation records visible to the requesting principal: the
// owner-scoped live set on the agent ring (another principal's relations stay
// invisible, a missing principal sees nothing), the shared live set
// elsewhere, always after forget-replay. Both Recall and the multi-hop graph
// read through here so the walk never sees a hidden edge.
func (r RelationRing) live() ([]kb.Relation, error) {
	if r.Path != "" {
		if err := requireRingDir(r.Path); err != nil {
			return nil, err
		}
	}
	if r.RingName == RingAgent {
		return (kb.OwnerRelationRing{Dir: r.Path, Owner: r.Owner}).Live()
	}
	return (kb.RelationRing{Dir: r.Path}).Live()
}

func recallPages(ring string, store *kb.Store, path string, q Query) ([]Hit, error) {
	pages, relation, err := visiblePages(ring, store, path, q)
	if err != nil {
		return nil, err
	}
	hits := kb.Search(pages, kb.Query{
		Terms:       kb.Terms(q.Text),
		Repo:        q.Repo,
		OS:          q.OS,
		K:           q.k(),
		MinCoverage: q.MinCoverage,
		// Status-superseded pages were already resolved against AsOf
		// above; one superseded only AFTER as-of is a valid hit here.
		All: true,
	})
	var out []Hit
	for _, h := range hits {
		if hit, ok := pageHit(ring, store, q, relation, h.Page, h.Score, h.Matched); ok {
			out = append(out, hit)
		}
	}
	return out, nil
}

// visiblePages returns the pages the requesting principal may see, filtered
// exactly as ranking sees them: forms and --memory types first (independent
// corpora, invisible to IDF), then lifecycle at the as-of instant (a
// forgotten or superseded page cannot shape what is returned), then the agent
// ring's lineage gate. The multi-hop graph builds from this pool, so a hidden
// page can never bridge, score, or appear — it is absent before ranking, not
// demoted after it. The relation map classifies agent-ring episodes for the
// checkpoint/working rules pageHit applies.
func visiblePages(ring string, store *kb.Store, path string, q Query) ([]*kb.Page, map[string]string, error) {
	if path != "" {
		if err := requireRingDir(path); err != nil {
			return nil, nil, err
		}
	}
	if store == nil {
		return nil, nil, nil
	}
	pages, err := store.List()
	if err != nil {
		return nil, nil, fmt.Errorf("kb: open %s: %w", path, err)
	}
	// Filter before ranking: forms (and --memory types) are independent
	// corpora, just as scoped-out pages are. Letting excluded records affect
	// IDF would change a requested form's order based on records that cannot
	// be returned. The type filter applies to kb pages only: relation records
	// and capability folds carry no OKF page type and are gated by the
	// ring/form selection instead.
	pages = slices.DeleteFunc(pages, func(p *kb.Page) bool {
		return !q.allowsForm(p.EffForm())
	})
	if len(q.Types) > 0 {
		pages = slices.DeleteFunc(pages, func(p *kb.Page) bool {
			return !slices.Contains(q.Types, p.Type)
		})
	}
	// Lifecycle before ranking, like forms: resolve every page at the as-of
	// instant (kblife) and keep only the valid ones, so a forgotten or
	// superseded page cannot shape the IDF of what is returned.
	tombs, err := store.Tombstones()
	if err != nil {
		return nil, nil, fmt.Errorf("kb: open %s: %w", path, err)
	}
	pages = kb.VisibleAt(pages, tombs, q.asOf())
	var relation map[string]string
	if ring == RingAgent {
		// Gate before ranking, like forms: an invisible note must not shape
		// the IDF of the ones that can be returned.
		relation = lineageOf(pages, q.Lineage)
		pages = slices.DeleteFunc(pages, func(p *kb.Page) bool {
			return !lineageVisible(p, relation, q.Lineage)
		})
	}
	return pages, relation, nil
}

// pageHit renders one visible page as a hit, applying the read-time gates
// ranking does not own: the --since age filter and the agent ring's
// checkpoint/working rules. False means the page stays invisible to this
// reader.
func pageHit(ring string, store *kb.Store, q Query, relation map[string]string, p *kb.Page, score float64, matched int) (Hit, bool) {
	if q.Since > 0 && !withinSince(p.Updated, p.Created, q.now(), q.Since) {
		return Hit{}, false
	}
	episode := ""
	if p.Source != nil {
		episode = p.Source.Episode
	}
	checkpoint := hasCheckpointTag(p.Tags)
	rel := relation[episode]
	if ring == RingAgent && checkpoint && rel == "" && (q.Episode == "" || q.Episode != episode) {
		return Hit{}, false
	}
	working := ring == RingAgent && episode != "" &&
		(rel == relSelf || rel == relAncestor || episode == q.Episode)
	return Hit{
		Ring: ring, Kind: p.Type, ID: "kb:" + p.Slug, Form: p.EffForm(),
		Cue: p.Title, Gist: p.Description, Full: p.Body,
		Source: []SourceRef{{URI: "file://" + store.PagePath(p.Slug)}},
		Status: p.Status, Confidence: "ASSERTED",
		ObservedAt: p.Updated, ValidFrom: validFrom(p), ValidTo: p.ValidTo,
		Score:   score,
		Why:     []string{"kb", "matched " + itoa(matched) + " terms"},
		Episode: episode, Checkpoint: checkpoint,
		Relation: rel, Working: working,
	}, true
}

func requireRingDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("kb: open %s: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("kb: open %s: not a directory", path)
	}
	return nil
}

func hasCheckpointTag(tags []string) bool {
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "checkpoint" || strings.HasPrefix(tag, "checkpoint:") {
			return true
		}
	}
	return false
}

const (
	relSelf       = "self"
	relAncestor   = "ancestor"
	relDescendant = "descendant"
)

// lineageOf classifies every episode on the agent ring's notes against the
// reader's lineage (see Lineage). Nil when the reader names no episode.
func lineageOf(pages []*kb.Page, l Lineage) map[string]string {
	self := strings.TrimSpace(l.Self)
	if self == "" {
		return nil
	}
	parent := map[string]string{}
	if p := strings.TrimSpace(l.Parent); p != "" && p != self {
		parent[self] = p
	}
	// The first recorded edge wins; pages arrive in a stable order, so the
	// chain is deterministic even if two notes disagree.
	for _, p := range pages {
		if p.Source == nil || p.Source.Episode == "" || p.Source.ParentEpisode == "" {
			continue
		}
		if _, ok := parent[p.Source.Episode]; !ok && p.Source.Episode != p.Source.ParentEpisode {
			parent[p.Source.Episode] = p.Source.ParentEpisode
		}
	}
	rel := map[string]string{self: relSelf}
	for ep := parent[self]; ep != ""; ep = parent[ep] {
		if _, seen := rel[ep]; seen {
			break // a cycle in recorded edges: stop, never loop
		}
		rel[ep] = relAncestor
	}
	// Descendants: every episode whose chain reaches self through children.
	for changed := true; changed; {
		changed = false
		for child, par := range parent {
			if _, seen := rel[child]; seen {
				continue
			}
			if r := rel[par]; r == relSelf || r == relDescendant {
				rel[child] = relDescendant
				changed = true
			}
		}
	}
	return rel
}

// lineageVisible applies the env-style rule: a child reads its ancestors'
// exported notes only, and a parent reads descendants only by --lineage.
func lineageVisible(p *kb.Page, rel map[string]string, l Lineage) bool {
	if rel == nil || p.Source == nil {
		return true
	}
	switch rel[p.Source.Episode] {
	case relAncestor:
		return p.Export
	case relDescendant:
		return l.Descendants
	}
	return true
}

// validFrom is the page's recorded interval start, else its creation time
// (the historical value, for pages written before intervals existed).
func validFrom(p *kb.Page) string {
	if p.ValidFrom != "" {
		return p.ValidFrom
	}
	return p.Created
}
