// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package spacegraph

import (
	"crypto/sha1"
	"encoding/hex"
	"time"

	"github.com/qiangli/yoke/pkg/craft"
)

// Schema tags every appended observation.
const Schema = "bashy-spacegraph-v1"

// Relation names an observed connection between two entities.
type Relation string

const (
	// RelReached — the source got to the target. The workhorse: every
	// successful network command asserts it.
	RelReached Relation = "reached"

	// RelAuthenticatedAs — the account a connection used. Carried as the edge's
	// Via rather than as a second edge, because "reached X" and "reached X as
	// Y" are one observation, not two.
	RelAuthenticatedAs Relation = "authenticates_as"

	// RelResolvesTo — a name that stands for another name, address or locality.
	RelResolvesTo Relation = "resolves_to"

	// RelRunsOn — a service or endpoint observed on a host. It is what keeps
	// `remote.host:2222` and `remote.host:22` distinct while still recording
	// that they are the same machine.
	RelRunsOn Relation = "runs_on"

	// RelContains — a repo and a path within it. The cross-link into the code
	// graph.
	RelContains Relation = "contains"
)

// Edge is one observed relation, accumulated over every time it was seen.
//
// The identity is (Src, Rel, Dst). Everything else is evidence about that one
// relation, which is what makes the seventh `ssh` to a host strengthen an edge
// instead of creating a seventh one.
type Edge struct {
	Schema string `json:"schema"`

	Src string   `json:"src"` // craft.Entity.ID()
	Rel Relation `json:"rel"`
	Dst string   `json:"dst"`

	// Via is the account the relation went through, when there was one. It is
	// an attribute rather than a node-to-node edge because it qualifies THIS
	// relation: the same pair reached under two logins is two facts about one
	// connection, and splitting them would hide that the host is reachable.
	Via string `json:"via,omitempty"`

	// Evidence. N counts observations, OK counts the ones that succeeded.
	// Both are needed: an edge seen 40 times and working twice is a different
	// claim from one seen twice and working twice, and an average hides it.
	N  int `json:"n"`
	OK int `json:"ok"`

	First time.Time `json:"first"`
	Last  time.Time `json:"last"`

	// Where it was observed. A relation true on one coordinate is not
	// automatically true on another, and recording this is what lets that be
	// checked rather than assumed.
	Coordinate string `json:"coordinate,omitempty"`
	Place      string `json:"place,omitempty"`

	// ValidFrom / ValidUntil are when the relation held in the WORLD, as
	// opposed to when this host wrote it down. Two timelines are what make
	// "what did this machine believe last Tuesday" answerable — and answering
	// that is the only way to explain why an agent did what it did.
	ValidFrom  time.Time  `json:"valid_from"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`

	Source string `json:"source,omitempty"` // what taught it: "exec:ssh"
}

// ID is the edge's stable key.
//
// Note what is NOT in it: no timestamp, no count, no episode, no duration. An
// edge that re-keyed on every observation would file each one at a fresh
// address and the store would fill with singletons while reporting no error at
// all. Time and counts are attributes, and they stay attributes.
func (e Edge) ID() string {
	sum := sha1.Sum([]byte(e.Src + "\x00" + string(e.Rel) + "\x00" + e.Dst))
	return hex.EncodeToString(sum[:])[:16]
}

// Live reports whether the edge is still believed at t.
func (e Edge) Live(t time.Time) bool {
	if e.ValidUntil != nil && !t.Before(*e.ValidUntil) {
		return false
	}
	return !t.Before(e.ValidFrom)
}

// Node is one entity in the graph.
//
// Nodes are DERIVED from edges rather than stored: an entity nothing connects
// to is not knowledge about the environment, it is a name someone typed once.
type Node struct {
	ID   string           `json:"id"`
	Kind craft.EntityKind `json:"kind"`
	Name string           `json:"name"`

	Out int `json:"out"`
	In  int `json:"in"`

	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

func nodeOf(id string) Node {
	kind, name := splitID(id)
	return Node{ID: id, Kind: kind, Name: name}
}

func splitID(id string) (craft.EntityKind, string) {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return craft.EntityKind(id[:i]), id[i+1:]
		}
	}
	return "", id
}
