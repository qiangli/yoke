// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/policy/audit"
	"github.com/qiangli/yoke/pkg/principal"
)

// Use is something the caller is about to touch: a kind and name, optionally
// narrowed to one member (a file inside a repo, a host inside a fleet).
type Use struct{ Kind, Name, Member string }

// Guard answers "may holder touch these?" without taking anything. It returns a
// *Conflict naming the first live claim, held by someone else, that the use
// falls under per the kind's match rule and domain. The holder's own claims
// pass, and announce-mode claims are advisory: they never block a Guard, only
// a competing Acquire.
func Guard(ctx context.Context, holder principal.Ref, uses ...Use) error {
	return store{DefaultDir()}.guard(ctx, holder, uses)
}

func (s store) guard(ctx context.Context, holder principal.Ref, uses []Use) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	all, err := List(s.dir)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, u := range uses {
		ref := normRef(Ref{Kind: u.Kind, Name: u.Name})
		kind := kindOrDefault(ref.Kind)
		var members []string
		switch {
		case u.Member != "":
			members = []string{u.Member}
		case kind.Match != MatchName && ref.Name != "":
			members = []string{ref.Name}
		}
		cands := all
		if b := customBackend(ref.Kind); b != nil {
			c, err := b.Load(ref.String())
			if err != nil {
				return err
			}
			if c != nil {
				cands = append(append([]*Claim(nil), all...), c)
			}
		}
		for _, c := range cands {
			if c.Mode == ModeAnnounce || sameHolder(c.Holder, holder) || c.Liveness(now).Takeable() {
				continue
			}
			c.normalize()
			sameKey := u.Member == "" && c.Resource != "" && c.Kind == ref.Kind && c.Resource == ref.Name
			if sameKey || kindsConflict(kind, ref.Name, members, kindOrDefault(c.Kind), c.name(), c.Members) {
				return &Conflict{Claim: c}
			}
		}
	}
	return nil
}

// auditForce records an acquisition that went past live claims held by others.
// An override nobody can see is an override nobody can audit, so the record is
// written for every forced pass; a log that cannot be written does not stop the
// human who said "I know, do it anyway".
func auditForce(ref Ref, r Request, displaced []*Claim) {
	argv := []string{"claim", "force", ref.String()}
	var who []string
	for _, c := range displaced {
		who = append(who, c.Holder.Name+"@"+c.Ref().String())
	}
	sort.Strings(who)
	argv = append(argv, "displaced="+strings.Join(who, ","))
	if r.Intent != "" {
		argv = append(argv, "intent="+r.Intent)
	}
	actor := audit.ActorFromEnv()
	if actor.Agent == "" {
		actor.Agent = r.Holder.Name
	}
	if actor.Session == "" {
		actor.Session = r.Holder.Episode
	}
	host, _ := os.Hostname()
	_, _ = audit.Append(audit.Record{
		Actor:    actor,
		Action:   "claim.force",
		Argv:     argv,
		Binary:   "bashy claim",
		Host:     host,
		Decision: "allow",
		Effects:  []string{"claim:force"},
	})
}
