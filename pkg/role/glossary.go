// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package role

import "strings"

// GlossaryEntry is one official role or occupancy in the binary glossary.
// It is generated from existing official vocabulary (role.Kind) plus the
// deputy occupancy, not hand-written prose.
type GlossaryEntry struct {
	Name            string            `json:"name"`
	Title           string            `json:"title"`
	Kind            Kind              `json:"kind,omitempty"`
	Scope           string            `json:"scope"`
	Occupancy       string            `json:"occupancy"`
	Description     string            `json:"description"`
	Address         string            `json:"address"`
	AliasesByContext map[string]string `json:"aliases_by_context,omitempty"`
}

// Glossary returns the single binary role glossary built from existing
// official vocabulary plus deputy occupancy. Official names are
// steward, deputy, conductor and worker; everyday words are shown by context.
func Glossary() []GlossaryEntry {
	return []GlossaryEntry{
		{
			Name:        "steward",
			Title:       "steward",
			Kind:        Steward,
			Scope:       "one machine × one OS account",
			Occupancy:   "Authority{Holder,Epoch} + heartbeat, tri-state liveness, epoch fencing, authorized Takeover (yoke/pkg/steward/)",
			Description: "The one steward per host×user who answers for the host. Holds the seat, runs the journal, and is the only seat that may allocate across scopes, release cross-scope resources, and integrate across deputies.",
			Address:     "steward",
			AliasesByContext: map[string]string{
				"host":    "steward",
				"manager": "steward (when talking about the host)",
			},
		},
		{
			Name:        "deputy",
			Title:       "deputy",
			Kind:        Steward, // occupancy of the steward position, not another Kind
			Scope:       "listed sprints or an epic, non-overlapping, time-boxed",
			Occupancy:   "Holder instance UUID, OnBehalfOf steward, non-overlapping scope (listed sprints or an epic), time box, revocation and steward epoch fencing. Several per host×user.",
			Description: "An occupancy of the steward position. May perform the steward's four acts only within its scope: activate scope transfer, fence, judge and run the merge gate. Cross-scope allocation/release/integration stays with the steward. Flat — no deputies of deputies, and a deputy cannot conduct a sprint in its own scope. Addressed as deputy:<scope> via existing inbox/mb/ping/meet.",
			Address:     "deputy:<scope>",
			AliasesByContext: map[string]string{
				"manager":   "deputy (when talking about managing several sprints)",
				"supervisor": "deputy in prose; official is deputy",
				"lead":      "deputy in prose; official is deputy",
			},
		},
		{
			Name:        "conductor",
			Title:       "conductor",
			Kind:        Conductor,
			Scope:       "one sprint",
			Occupancy:   "weaveStoryLease{Holder,At}, 30 min TTL (yoke/pkg/weave/weave_story.go)",
			Description: "Holds one sprint's lease and delivers that sprint. One writer per sprint; N sibling conductors under one steward is the flat scaling pattern.",
			Address:     "conductor:<sprint>",
			AliasesByContext: map[string]string{
				"manager":       "conductor (when talking about managing a sprint)",
				"sprint manager": "conductor",
			},
		},
		{
			Name:        "worker",
			Title:       "worker",
			Kind:        "",
			Scope:       "one run",
			Occupancy:   "no seat — addressed through its conductor",
			Description: "Catch-all for agents doing the work: a weave agent, a foreman sub-hub, or a harness-internal subagent. Every actor is either an addressable seat or somebody's worker, with nothing in between.",
			Address:     "(via conductor)",
			AliasesByContext: map[string]string{
				"agent": "worker in prose; official is worker",
			},
		},
	}
}

// GlossaryByName returns one entry by official name, case-insensitive.
func GlossaryByName(name string) (GlossaryEntry, bool) {
	for _, e := range Glossary() {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return GlossaryEntry{}, false
}
