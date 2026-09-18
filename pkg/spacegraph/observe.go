// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package spacegraph

import (
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/craft"
)

// Context is where and when an observation was made.
type Context struct {
	// Self is this machine's hostname — the node every `reached` edge starts
	// from. Without it the graph has no origin and "what can I get to from
	// here" is unanswerable.
	Self string

	Repo    string // repo identity, if the command ran inside one
	Subpath string // repo-relative directory, already coarse

	Coordinate string
	Place      string
	At         time.Time
	Source     string
}

// Observe turns one invocation into the relations it revealed.
//
// It reads only what a command DECLARES, via craft's per-binary role table. An
// unknown binary yields nothing rather than a guess — inventing an edge from a
// flag shape is how `-p` becomes "port" for a command where it means
// "preserve", and a wrong edge in a store an agent trusts is worse than none.
//
// The ok argument is the caller's verdict, not an exit code. A transport
// failure must arrive as ok=false AND be dropped by the caller; see the package
// doc on why failure teaches nothing.
func Observe(argv []string, ok bool, ctx Context) []Observation {
	if len(argv) == 0 || ctx.Self == "" {
		return nil
	}
	at := ctx.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	self := craft.Entity{Kind: craft.EntityHost, Name: ctx.Self}.ID()

	mk := func(src string, rel Relation, dst, via string) Observation {
		return Observation{
			Src: src, Rel: rel, Dst: dst, Via: via, OK: ok, At: at,
			Coordinate: ctx.Coordinate, Place: ctx.Place, Source: ctx.Source,
		}
	}

	var out []Observation

	// The network half — this is the shape the whole plane exists for.
	if x, _ := craft.Extract(argv); x.Entity.Valid() && x.Entity.Kind == craft.EntityHost {
		host := x.Entity
		hostID := host.ID()

		// Never record the local machine as something it reached. A self-edge
		// is not a fact about the environment.
		if !strings.EqualFold(strings.TrimSpace(host.Name), strings.TrimSpace(ctx.Self)) {
			via := ""
			if user := x.Roles[craft.RoleUser]; user != "" {
				via = craft.Entity{
					Kind: craft.EntityAccount,
					Name: user + "@" + host.Name,
				}.ID()
			}

			dst := hostID
			if port := x.Roles[craft.RolePort]; port != "" {
				// The port is part of what was reached: a host answering on
				// 2222 and the same host answering on 22 are different facts,
				// and collapsing them loses the one worth remembering.
				endpoint := craft.Entity{
					Kind: craft.EntityEndpoint,
					Name: host.Name + ":" + port,
				}.ID()
				out = append(out, mk(endpoint, RelRunsOn, hostID, ""))
				dst = endpoint
			}
			out = append(out, mk(self, RelReached, dst, via))
		}
	}

	// The workspace half — which parts of which repo get worked in. Bounded by
	// the directories anyone actually cd's into, so it stays small.
	if ctx.Repo != "" && ctx.Subpath != "" {
		repo := craft.Entity{Kind: craft.EntityRepo, Name: ctx.Repo}.ID()
		path := craft.Entity{Kind: craft.EntityPath, Name: ctx.Subpath}.ID()
		out = append(out, mk(repo, RelContains, path, ""))
	}

	// The locality half — which networks this machine has worked from. One
	// edge per place, and the place is a fingerprint rather than an address, so
	// "the same wifi as last week" is expressible without recording which wifi.
	if ctx.Place != "" {
		net := craft.Entity{Kind: craft.EntityNet, Name: ctx.Place}.ID()
		out = append(out, mk(self, RelResolvesTo, net, ""))
	}

	return out
}

// RecordAll folds a batch of observations into the store.
//
// Errors are returned rather than swallowed here; the CALLER on the hot path is
// the one that must treat a failed write as a missed lesson rather than a
// failed command.
func (s *Store) RecordAll(obs []Observation) error {
	for _, o := range obs {
		if err := s.Record(o); err != nil {
			return err
		}
	}
	return nil
}
