package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/qiangli/yoke/pkg/ref"
)

// EntitySchemes are the agent-world locator schemes resolved through a
// ref.Registry. Every one names a closed-vocabulary ref kind, so Stat and
// List dispatch to the owning store's already-registered resolver; this
// package adds no kinds, no parsing and no store reads of its own.
var EntitySchemes = []string{"sprint", "run", "agent", "mb", "meet", "bus", "kb", "host"}

// UnwiredSchemes require an explicit Resolver instead of a ref.Registry kind.
var UnwiredSchemes = []string{"story", "claim"}

// AgentWorldSchemes is the full locator-scheme set cmds/lens registers: the
// resolver-backed EntitySchemes followed by the unwired seam schemes.
var AgentWorldSchemes = []string{
	"sprint", "run", "agent", "mb", "meet", "bus", "kb", "host",
	"story", "claim",
}

// Entity is a read-only provider over agent-world records. It resolves one
// locator scheme through a shared ref.Registry: the registry answers, the
// provider only shapes the row. A nil Reg reads as an empty registry — every
// ref-kind lookup reports ref.ErrNoResolver, never "not found".
type Entity struct {
	Reg    *ref.Registry
	Scheme string
	// Resolver overrides registry dispatch, including locator-only story/claim.
	Resolver ref.Resolver
}

// NewEntity returns the provider for scheme reading through reg.
func NewEntity(reg *ref.Registry, scheme string) *Entity {
	return &Entity{Reg: reg, Scheme: scheme}
}

// check rejects what this provider cannot serve before any store is read.
// Inner descent and location scopes need container and tier transports that
// v1 does not have; a scheme outside the closed ref vocabulary has no
// resolver to call, which is reported as unsupported rather than silently
// reinterpreted.
func (e *Entity) check(loc ref.Locator) error {
	if loc.Scheme != e.Scheme {
		return fmt.Errorf("provider: entity scheme %q cannot serve %q", e.Scheme, loc.Scheme)
	}
	if len(loc.Inner) != 0 {
		return fmt.Errorf("provider: %s: container descent (!/) is not supported for records: %w", loc.Scheme, ErrUnsupported)
	}
	if loc.ScopeKind != "" {
		return fmt.Errorf("provider: %s: location scopes are not supported for records yet: %w", loc.Scheme, ErrUnsupported)
	}
	if e.Resolver == nil && !ref.Known(ref.Kind(e.Scheme)) {
		return fmt.Errorf("provider: scheme %q is a locator scheme, not a ref kind: no owning-store resolver is wired (the closed ref vocabulary is unchanged): %w", e.Scheme, ErrUnsupported)
	}
	if loc.Target == "" {
		return fmt.Errorf("provider: %s: %w", e.Scheme, ref.ErrEmptyID)
	}
	return nil
}

// resolve asks the owning store's resolver for loc.Target. The resolver's
// error passes through verbatim: ref.ErrNotFound stays "the store was read
// and the id is absent", any other error stays "the store could not be read",
// and a kind nobody wired stays ref.ErrNoResolver. The three are never
// conflated.
func (e *Entity) resolve(loc ref.Locator) (ref.Node, error) {
	if err := e.check(loc); err != nil {
		return ref.Node{}, err
	}
	res := e.Resolver
	if res == nil {
		res, _ = e.Reg.Lookup(ref.Kind(e.Scheme))
	}
	if res == nil {
		return ref.Node{}, fmt.Errorf("%w: %s", ref.ErrNoResolver, e.Scheme)
	}
	n, err := res.Resolve(loc.Target)
	if err != nil {
		return ref.Node{}, err
	}
	if n.Ref == "" {
		n.Ref = ref.Format(n.Kind, n.ID)
	}
	return n, nil
}

// entityNode shapes a resolved record as a bashy-node-v1 row. The record is
// a leaf (Kind "record", never "directory", so walks stop here); its child
// count is omitted, which means unknown, not zero. Store-specific title,
// status, successor and handles are retained in attrs; absent optional
// fields are omitted.
func entityNode(loc ref.Locator, rn ref.Node) Node {
	name := rn.Title
	if name == "" {
		name = rn.ID
	}
	attrs := map[string]any{"kind": string(rn.Kind), "id": rn.ID}
	if rn.Title != "" {
		attrs["title"] = rn.Title
	}
	if rn.Status != "" {
		attrs["status"] = rn.Status
	}
	if rn.Where != "" {
		attrs["where"] = rn.Where
	}
	if rn.Open != "" {
		attrs["open"] = rn.Open
	}
	if rn.Successor != "" {
		attrs["successor"] = rn.Successor
	}
	if rn.UID != "" {
		attrs["uid"] = rn.UID
	}
	if rn.Seq != 0 {
		attrs["seq"] = rn.Seq
	}
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          loc.Target,
		Name:          name,
		Kind:          "record",
		Type:          loc.Scheme,
		Attrs:         attrs,
	}
}

// Stat resolves the record. A missing id reports ref.ErrNotFound; a store
// that could not be read reports its own error; both stay distinguishable
// with errors.Is.
func (e *Entity) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	rn, err := e.resolve(loc)
	if err != nil {
		return Node{}, err
	}
	return entityNode(loc, rn), nil
}

// List resolves the record first — a missing id is ref.ErrNotFound, not an
// empty success — then reports its known children: none, in v1.
func (e *Entity) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := e.resolve(loc); err != nil {
		return nil, err
	}
	return []Node{}, nil
}

// Open is unsupported: records render through the listing, tree and JSON
// views (typed interchange), not as a byte stream.
func (e *Entity) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.check(loc); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("provider: %s: records have no byte stream (use the listing or tree view): %w", e.Scheme, ErrUnsupported)
}

// Edges is unsupported: records expose no graph relationships in v1.
func (e *Entity) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.check(loc); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("provider: %s: records expose no graph edges: %w", e.Scheme, ErrUnsupported)
}
