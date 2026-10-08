// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package role

import (
	"sort"
	"strings"
)

// The official names are steward, deputy, conductor and worker
// (docs/orchestration-roles.md §3a). Only steward and conductor are a Kind;
// deputy is an OCCUPANCY of the steward position and worker holds no seat, so
// neither adds a Kind. Everyday words ("manager", "lead", …) are read in
// context, never policed.
const (
	Deputy = "deputy"
	Worker = "worker"
)

// Kinds is the official title vocabulary.
func Kinds() []Kind { return []Kind{Steward, Conductor} }

// GlossaryEntry is one official name in the binary glossary.
type GlossaryEntry struct {
	Name             string            `json:"name"`
	Kind             Kind              `json:"kind,omitempty"` // the title it is (or occupies)
	Occupancy        bool              `json:"occupancy,omitempty"`
	Scope            string            `json:"scope"`
	Address          string            `json:"address"`
	Holds            string            `json:"holds"`
	Description      string            `json:"description"`
	AliasesByContext map[string]string `json:"aliases_by_context,omitempty"`
}

// AliasContexts lists the everyday words in a stable order.
func (e GlossaryEntry) AliasContexts() []string {
	out := make([]string, 0, len(e.AliasesByContext))
	for k := range e.AliasesByContext {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Glossary is generated from the vocabulary above: one entry per Kind, the
// deputy occupancy of the steward Kind, and worker. Addresses come from
// Assignment.Label so the glossary cannot drift from what addressing accepts.
func Glossary() []GlossaryEntry {
	var out []GlossaryEntry
	for _, k := range Kinds() {
		switch k {
		case Steward:
			out = append(out, GlossaryEntry{
				Name: string(k), Kind: k,
				Scope:       "one machine × one OS account",
				Address:     Assignment{Kind: k}.Label(),
				Holds:       "the steward seat: holder, epoch fence, journal",
				Description: "answers for the host; the only seat that allocates across scopes, releases and integrates, and grants deputies",
				AliasesByContext: map[string]string{
					"manager": "the steward, when talking about the host",
				},
			}, GlossaryEntry{
				Name: Deputy, Kind: k, Occupancy: true,
				Scope:       "listed sprints or one epic; non-overlapping; time-boxed",
				Address:     Deputy + ":<scope>",
				Holds:       "an occupancy of the steward seat: instance UUID, on behalf of the steward, fenced by its epoch",
				Description: "performs the steward's four acts (activate, fence, judge, merge gate) inside its scope; never conducts there; no deputies of deputies",
				AliasesByContext: map[string]string{
					"manager":    "a deputy, when talking about managing several sprints",
					"supervisor": "a deputy, in prose",
					"lead":       "a deputy, in prose",
				},
			})
		case Conductor:
			out = append(out, GlossaryEntry{
				Name: string(k), Kind: k,
				Scope:       "one sprint",
				Address:     Assignment{Kind: k, Ref: "<sprint>"}.Label(),
				Holds:       "the sprint lease",
				Description: "delivers one sprint; escalates to its deputy, or to the steward",
				AliasesByContext: map[string]string{
					"manager":        "the conductor, when talking about managing a sprint",
					"sprint manager": "the conductor",
				},
			})
		}
	}
	return append(out, GlossaryEntry{
		Name:        Worker,
		Scope:       "one run",
		Address:     "(through its conductor)",
		Holds:       "no seat",
		Description: "an agent doing the work under a conductor",
		AliasesByContext: map[string]string{
			"agent": "a worker, in prose",
		},
	})
}

// GlossaryByName returns one entry by official name, case-insensitive.
func GlossaryByName(name string) (GlossaryEntry, bool) {
	for _, e := range Glossary() {
		if strings.EqualFold(e.Name, strings.TrimSpace(name)) {
			return e, true
		}
	}
	return GlossaryEntry{}, false
}
