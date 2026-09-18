// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

// The ref resolver: todo answers `todo:<id>` for the uniform addressing grammar
// (pkg/ref). Registration is one call the embedding shell makes; todo keeps
// resolving only its own kind.
//
// A todo is an ENTITY with three handles (ref.Shape): its 12-hex id (or a
// git-style unique PREFIX — the identity, what `todo show` takes), its running
// number (`todo:148`, `#148` — what `todo show 148` means), and the slug half of
// its filename (`todo:ref-shapes-kb-id-seq-…`). An optional scope segment names
// the store: `todo:coreutils/148` is #148 in the coreutils checkout's list,
// `todo:user/3` is #3 on the personal list; without one the store is chosen
// the way the CLI chooses it (the cwd's repo). The distinction the ref contract
// requires: an id that matches NOTHING is ErrNotFound (a fact about the
// record), but an AMBIGUOUS prefix or slug is a plain error naming the
// candidates — never ErrNotFound, because the record is not absent, the query
// is under-specified. The resolved Node always carries the FULL id, even when a
// prefix or a seq was given; seq is accepted as input, never emitted as a ref.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs installs the todo resolver on g. The arguments are the same store
// selectors the `todo` CLI takes (see ResolveStore): the store is resolved per
// lookup so the cwd's repo is honored the way the CLI honors it. scopes maps a
// leading `<scope>/` segment to a checkout root (nil = scoped refs are errors).
func RegisterRefs(g *ref.Registry, owner string, forceRepo, forceUser bool, baseDir string, scopes ref.ScopeLookup) {
	g.Register(ref.Todo, ref.ResolverFunc(func(id string) (ref.Node, error) {
		scope, local := ref.SplitScope(id)
		st, err := scopedStore(scope, owner, forceRepo, forceUser, baseDir, scopes)
		if err != nil {
			return ref.Node{}, err
		}
		return resolveTodo(st, local)
	}))
}

// scopedStore picks the store a scope segment names. No scope = today's CLI
// selection. "user" = the personal list. Any other name goes through the
// embedder's lookup; without one, a scoped ref is an error that names the scope
// — never a silent fall-through to the cwd, which would resolve the WRONG #N.
func scopedStore(scope, owner string, forceRepo, forceUser bool, baseDir string, scopes ref.ScopeLookup) (*issue.Store, error) {
	switch scope {
	case "":
		st, _, err := ResolveStore(owner, forceRepo, forceUser, baseDir)
		return st, err
	case "user":
		st, _, err := ResolveStore(owner, false, true, "")
		return st, err
	}
	if scopes == nil {
		return nil, fmt.Errorf("todo:%s/…: no scope lookup is wired — a scope names a checkout by basename (or `user`)", scope)
	}
	root, err := scopes(scope)
	if err != nil {
		return nil, fmt.Errorf("todo:%s/…: %w", scope, err)
	}
	if root == "" {
		st, _, err := ResolveStore(owner, false, true, "")
		return st, err
	}
	st, _, err := ResolveStore(owner, true, false, root)
	return st, err
}

// resolveTodo finds an item within one store by whichever handle the local part
// spells (ref.ShapeOf): seq → the running number; uid → exact id, then unique
// prefix; slug → the filename slug. It does the git-style match itself (rather
// than issue.Store.Resolve) so it can keep "absent" (ErrNotFound) distinct from
// "ambiguous" (a named error) — the store's Resolve collapses both.
func resolveTodo(st *issue.Store, id string) (ref.Node, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	items, err := st.List()
	if err != nil {
		return ref.Node{}, fmt.Errorf("todo: read %s: %w", st.Dir(), err)
	}
	var hits []*issue.Issue
	switch ref.ShapeOf(id) {
	case ref.ShapeSeq:
		n, _ := strconv.Atoi(id)
		for _, it := range items {
			if it.Seq == n {
				hits = append(hits, it)
			}
		}
	case ref.ShapeUID:
		// An exact id always wins over a prefix, exactly as `git show` and the
		// register do.
		for _, it := range items {
			if it.ID == id {
				return todoNode(it, st), nil
			}
		}
		for _, it := range items {
			if strings.HasPrefix(it.ID, id) {
				hits = append(hits, it)
			}
		}
	default:
		for _, it := range items {
			if it.Slug() == id {
				hits = append(hits, it)
			}
		}
	}
	if len(hits) == 0 && isHex(id) {
		// Today's CLI contract, kept: a hex string of ANY length is a prefix
		// (`todo show a1`), and a number with no such seq falls back to the
		// prefix match too (ResolveRef does the same).
		for _, it := range items {
			if strings.HasPrefix(it.ID, strings.ToLower(id)) {
				hits = append(hits, it)
			}
		}
	}
	switch len(hits) {
	case 0:
		return ref.Node{}, fmt.Errorf("todo:%s: %w", id, ref.ErrNotFound)
	case 1:
		return todoNode(hits[0], st), nil
	default:
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, fmt.Sprintf("#%d %s %s", h.Seq, h.ID, h.Title))
		}
		return ref.Node{}, fmt.Errorf("todo:%s is ambiguous — %d items match:\n  %s",
			id, len(hits), strings.Join(names, "\n  "))
	}
}

// todoNode renders an item as a ref.Node. The Node's id is the item's FULL id
// even when a prefix, a seq or a slug was resolved, so a ref is stable
// regardless of how it was typed; UID and Seq carry the other handles.
func todoNode(it *issue.Issue, st *issue.Store) ref.Node {
	n := ref.NewNode(ref.Todo, it.ID)
	n.Title = it.Title
	n.Status = it.Status
	n.Where = st.Dir()
	n.Open = "bashy todo show " + it.ID
	n.UID = it.ID
	n.Seq = int64(it.Seq)
	return n
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return s != ""
}
