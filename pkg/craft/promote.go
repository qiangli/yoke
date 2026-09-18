package craft

// PROMOTION — noticing that a particular has become a pattern.
//
// A fact is knowledge about one thing. But when the same thing is true of the
// third entity in a row, it has probably stopped being particular: "this
// service answers on 3000" said of one service is a fact; said of every service
// here, it is how this place works.
//
// That transition is the learning ladder's last rung, and it is the one step
// the system cannot take on its own evidence alone — which is why this proposes
// and never decides.
//
// # Propose; the gate disposes
//
// Promotion does NOT get to bypass the fold admission gate, and the interlock
// is the point. Consider the obvious repeated pattern: `remote_user =
// svc-build` on three hosts. It IS a real regularity — and the note stating it
// names a username, so the gate refuses it and it stays local.
//
// That is correct rather than an obstacle. A pattern being widespread on YOUR
// machines does not make it safe to share; it makes it a widespread local fact.
// Promotion can therefore never manufacture a leak, because everything it
// proposes goes through the same door everything else does.
//
// Candidates that would be refused are still REPORTED, with the reason. An
// operator learning "this repeats but cannot travel" has learned something; a
// candidate silently filtered out teaches nothing.
//
// # Why three
//
// Two is a coincidence. Three is the smallest number that distinguishes a
// pattern from a pair, and it is the threshold already used elsewhere in this
// tree (the promote-after-N rule in the weave recipe design, and the minimum
// cluster size the skill-library literature converged on). Reusing it beats
// inventing a fourth number.

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultPromotionMin is the number of distinct entities a fact must hold for
// before it is proposed as general.
const DefaultPromotionMin = 3

// PromotionCandidate is a repeated fact that may have stopped being particular.
type PromotionCandidate struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Entities is where it holds, sorted.
	Entities []Entity `json:"entities"`
	// Note is the fold this would become.
	Note string `json:"note"`
	// Blocked is why it cannot be promoted, empty when it can. A candidate
	// naming identity is reported rather than hidden: "this repeats but cannot
	// travel" is worth knowing.
	Blocked string `json:"blocked,omitempty"`
}

// Promotable reports whether this candidate can become a fold.
func (c PromotionCandidate) Promotable() bool { return c.Blocked == "" }

// PromotionCandidates finds facts that hold identically across at least min
// distinct entities.
//
// Matching is on (key, VALUE), not key alone. `port=3000` on three services is
// a pattern; three services on three different ports is three facts that happen
// to share a field name, and calling that a regularity would be reading
// structure into noise.
func (s *FactStore) PromotionCandidates(min int, folds *FoldStore) []PromotionCandidate {
	if min <= 0 {
		min = DefaultPromotionMin
	}

	// Group live facts by (key, value), collecting the entities they hold for.
	type group struct {
		key, value string
		entities   []Entity
		seen       map[string]bool
	}
	groups := map[string]*group{}
	for _, e := range s.Entities() {
		for _, f := range s.For(e) {
			k := f.Key + "\x00" + f.Value
			g, ok := groups[k]
			if !ok {
				g = &group{key: f.Key, value: f.Value, seen: map[string]bool{}}
				groups[k] = g
			}
			if !g.seen[e.ID()] {
				g.seen[e.ID()] = true
				g.entities = append(g.entities, e)
			}
		}
	}

	var out []PromotionCandidate
	for _, g := range groups {
		if len(g.entities) < min {
			continue
		}
		sort.Slice(g.entities, func(i, j int) bool { return g.entities[i].ID() < g.entities[j].ID() })
		c := PromotionCandidate{
			Key:      g.key,
			Value:    g.value,
			Entities: g.entities,
			Note:     promotionNote(g.key, g.value, g.entities),
		}
		// Ask the SAME gate that guards every other fold. Promotion earns no
		// exemption: a pattern being widespread on your machines does not make
		// it shareable, it makes it a widespread local fact.
		if folds != nil && folds.scrub != nil {
			if _, found := folds.scrub.Scrub(c.Note); len(found) > 0 {
				c.Blocked = (&ErrNotGeneralisable{Found: found}).Error()
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Entities) != len(out[j].Entities) {
			return len(out[i].Entities) > len(out[j].Entities) // strongest evidence first
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// promotionNote phrases the regularity mechanically.
//
// Mechanical on purpose: a model asked to phrase it would produce something
// smoother and occasionally something wrong, and a fold nobody verified is
// exactly what the gate exists to keep out. The wording also carries the
// EVIDENCE COUNT, so a reader can weigh the claim instead of taking it.
func promotionNote(key, value string, entities []Entity) string {
	kinds := map[EntityKind]int{}
	for _, e := range entities {
		kinds[e.Kind]++
	}
	subject := "things"
	if len(kinds) == 1 {
		for k := range kinds {
			subject = string(k) + "s"
		}
	}
	return fmt.Sprintf("%s is %s on all %d known %s here", key, value, len(entities), subject)
}

// Promote records a candidate as a fold at a coordinate.
//
// Returns the gate's own error when the candidate names identity, rather than a
// pre-checked refusal: there is one door, and this goes through it.
func Promote(c PromotionCandidate, coordinate string, folds *FoldStore) error {
	if folds == nil {
		return ErrNoStore
	}
	if strings.TrimSpace(coordinate) == "" {
		return fmt.Errorf("craft: promotion needs a coordinate — a fold that holds nowhere in particular holds nowhere")
	}
	names := make([]string, 0, len(c.Entities))
	for _, e := range c.Entities {
		names = append(names, e.ID())
	}
	return folds.Record(Fold{
		Coordinate: coordinate,
		Note:       c.Note,
		// The evidence is the observation itself: N entities, named. If the
		// claim is ever doubted, this says exactly what it rested on.
		Evidence: fmt.Sprintf("observed on %d entities: %s", len(c.Entities), strings.Join(names, ", ")),
		Source:   "promotion",
	})
}
