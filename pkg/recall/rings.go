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
}

func (r RelationRing) Ring() string { return r.RingName }
func (RelationRing) Forms() []string {
	return []string{kb.FormRelation}
}
func (r RelationRing) Recall(q Query) ([]Hit, error) {
	if r.Path != "" {
		if err := requireRingDir(r.Path); err != nil {
			return nil, err
		}
	}
	live, err := (kb.RelationRing{Dir: r.Path}).Live()
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

func recallPages(ring string, store *kb.Store, path string, q Query) ([]Hit, error) {
	if path != "" {
		if err := requireRingDir(path); err != nil {
			return nil, err
		}
	}
	if store == nil {
		return nil, nil
	}
	pages, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("kb: open %s: %w", path, err)
	}
	// Filter before ranking: forms are independent corpora, just as scoped-out
	// pages are. Letting excluded forms affect IDF would change a requested
	// form's order based on records that cannot be returned.
	if len(q.Forms) > 0 {
		pages = slices.DeleteFunc(pages, func(p *kb.Page) bool {
			return !slices.Contains(q.Forms, p.EffForm())
		})
	}
	hits := kb.Search(pages, kb.Query{
		Terms:       kb.Terms(q.Text),
		Repo:        q.Repo,
		OS:          q.OS,
		K:           q.k(),
		MinCoverage: q.MinCoverage,
	})
	var out []Hit
	for _, h := range hits {
		p := h.Page
		if q.Since > 0 && !withinSince(p.Updated, p.Created, q.now(), q.Since) {
			continue
		}
		episode := ""
		if p.Source != nil {
			episode = p.Source.Episode
		}
		checkpoint := hasCheckpointTag(p.Tags)
		if ring == RingAgent && checkpoint && (q.Episode == "" || q.Episode != episode) {
			continue
		}
		out = append(out, Hit{
			Ring: ring, Kind: p.Type, ID: "kb:" + p.Slug, Form: p.EffForm(),
			Cue: p.Title, Gist: p.Description, Full: p.Body,
			Source: []SourceRef{{URI: "file://" + store.PagePath(p.Slug)}},
			Status: p.Status, Confidence: "ASSERTED",
			ObservedAt: p.Updated, ValidFrom: p.Created,
			Score:   h.Score,
			Why:     []string{"kb", "matched " + itoa(h.Matched) + " terms"},
			Episode: episode, Checkpoint: checkpoint,
		})
	}
	return out, nil
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
