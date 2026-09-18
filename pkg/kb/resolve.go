package kb

// The ref resolver: kb answers `kb:<slug>` for the uniform addressing grammar
// (pkg/ref). Registration is one call the embedding shell makes; kb keeps
// resolving only its own kind, exactly as the leaf pin (TestKBIsALeaf) requires.
//
// The lookup mirrors `kb show`: the rings the caller can see, in the order kb
// reads them — the repo ring of the cwd first, then the host store. A superseded
// page still resolves (a ref is stable for the record's life, ref design D6):
// its Status is "superseded" and Node.Successor points forward to the page that
// replaced it, the pair supersede records (see publishSuperseded in bus.go).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

// kbRing is one store the resolver consults, with the short name that names it
// in a Node's Where ("repo" | "host" | "dir").
type kbRing struct {
	name  string
	store *Store
}

// RegisterRefs installs the kb resolver on g. dir, when non-empty, pins the
// store to that one directory (the CLI's --dir); empty resolves the way
// `kb show` auto-detects — the repo ring of the cwd first, then the host store.
// scopes maps a leading `<scope>/` segment of the id to a checkout root whose
// repo ring is then the ONLY ring consulted (`user` = the host store); nil
// means a scoped ref is an error naming the scope, never a silent fallback.
func RegisterRefs(g *ref.Registry, dir string, scopes ref.ScopeLookup) {
	g.Register(ref.KB, ref.ResolverFunc(func(id string) (ref.Node, error) {
		scope, local := ref.SplitScope(id)
		rings, err := scopedRings(dir, scope, scopes)
		if err != nil {
			return ref.Node{}, err
		}
		return resolveKB(rings, local)
	}))
}

// scopedRings picks the rings a scope segment names: none = today's order
// (kbRings); "user" = the host store; anything else = that checkout's repo
// ring via the embedder's lookup.
func scopedRings(dir, scope string, scopes ref.ScopeLookup) ([]kbRing, error) {
	switch scope {
	case "":
		return kbRings(dir), nil
	case "user":
		return []kbRing{{name: "host", store: Open(DefaultDir())}}, nil
	}
	if scopes == nil {
		return nil, fmt.Errorf("kb:%s/…: no scope lookup is wired — a scope names a checkout by basename (or `user`)", scope)
	}
	root, err := scopes(scope)
	if err != nil {
		return nil, fmt.Errorf("kb:%s/…: %w", scope, err)
	}
	if root == "" {
		return []kbRing{{name: "host", store: Open(DefaultDir())}}, nil
	}
	return []kbRing{{name: "repo", store: Open(filepath.Join(root, RepoSub))}}, nil
}

// resolveKB looks a handle (slug, seq or uuid — ref.ShapeOf) up across the
// visible rings in order. A ring where the page is simply absent is skipped; an
// ambiguous handle within a ring is a named error (the query is under-specified,
// the page is not absent); a ring that cannot be read is a hard error, never
// ErrNotFound — "not found" is a fact about the record, "cannot read" is a fact
// about this host, and the two must stay distinct. The Node's ref is always
// kb:<slug>: seq is accepted as input, never emitted.
func resolveKB(rings []kbRing, local string) (ref.Node, error) {
	local = strings.TrimPrefix(strings.TrimSpace(local), "#")
	for _, r := range rings {
		p, err := r.store.LoadByHandle(local)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // absent in this ring — try the next
			}
			if errors.Is(err, ErrAmbiguous) {
				return ref.Node{}, err
			}
			return ref.Node{}, fmt.Errorf("kb: read %s: %w", r.store.Dir(), err)
		}
		return kbNode(p, r), nil
	}
	return ref.Node{}, fmt.Errorf("kb:%s: %w", local, ref.ErrNotFound)
}

// kbRings returns the stores to consult, in read order. An explicit dir pins one
// store; otherwise the repo ring of the cwd (when in a git repo) comes first,
// then the host store.
func kbRings(dir string) []kbRing {
	if dir != "" {
		return []kbRing{{name: "dir", store: Open(dir)}}
	}
	var rings []kbRing
	if cwd, err := os.Getwd(); err == nil {
		if root := repoRootOf(cwd); root != "" {
			rings = append(rings, kbRing{name: "repo", store: Open(filepath.Join(root, RepoSub))})
		}
	}
	rings = append(rings, kbRing{name: "host", store: Open(DefaultDir())})
	return rings
}

// kbNode renders a page as a ref.Node. A superseded page carries its Successor —
// the ref of the page that replaced it — so a reader that resolves a stale ref is
// pointed forward rather than left believing an invalidated fact.
func kbNode(p *Page, r kbRing) ref.Node {
	n := ref.NewNode(ref.KB, p.Slug)
	n.Title = p.Title
	n.Status = p.Status
	n.Where = r.name + " " + r.store.Dir()
	n.Open = "bashy kb show " + p.Slug
	n.UID = p.ID
	n.Seq = int64(p.Seq)
	if p.Status == StatusSuperseded && p.SupersededBy != "" {
		n.Successor = ref.Format(ref.KB, p.SupersededBy)
	}
	return n
}
