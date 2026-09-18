package principal

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs installs this package's resolvers into the uniform-ref
// registry: person and role, answered by the same Resolver whois builds
// from these options.
func RegisterRefs(g *ref.Registry, opts ...fleet.Option) {
	r := NewResolver(fleet.New(opts...), DefaultEnv())
	g.Register(ref.Person, ref.ResolverFunc(r.refPerson))
	g.Register(ref.Role, ref.ResolverFunc(r.refRole))
}

func (r *Resolver) refPerson(id string) (ref.Node, error) {
	res, ok := r.resolvePerson(id)
	if !ok {
		return ref.Node{}, fmt.Errorf("principal: person %q: %w", id, ref.ErrNotFound)
	}
	n := ref.NewNode(ref.Person, res.Name)
	n.Title = firstNonEmpty(res.Display, res.Summary)
	n.Where = res.Source
	n.Open = "bashy whois " + res.Name
	return n, nil
}

// refRole answers for a seat, by label or by its topic — the same two
// spellings the addresser and resolveRole accept. The holder is the point
// of the answer (see HostRole.Holder), so the title names it or says the
// seat is vacant; the status is the one-word version of the same fact.
func (r *Resolver) refRole(id string) (ref.Node, error) {
	name := strings.TrimSpace(id)
	if name == "" {
		return ref.Node{}, fmt.Errorf("principal: role: %w", ref.ErrNotFound)
	}
	for _, hr := range hostRoles() {
		if !strings.EqualFold(hr.Label, name) && !strings.EqualFold(hr.Topic, name) {
			continue
		}
		n := ref.NewNode(ref.Role, hr.Label)
		if h := strings.TrimSpace(hr.Holder); h != "" {
			n.Title = "held by " + h
			n.Status = "held"
		} else {
			n.Title = "vacant seat"
			n.Status = "vacant"
		}
		n.Where = hr.Topic
		n.Open = "bashy whois role:" + hr.Label
		return n, nil
	}
	return ref.Node{}, fmt.Errorf("principal: role %q: %w", id, ref.ErrNotFound)
}

// canonicalRef is the uniform `<kind>:<id>` spelling for a resolution. The
// principal kinds are all in the ref vocabulary by construction; anything
// else (defensively) yields no ref rather than a misfiled one.
func canonicalRef(res Resolution) string {
	k := ref.Kind(res.Kind)
	if !ref.Known(k) || res.Name == "" {
		return ""
	}
	return ref.Format(k, res.Name)
}
