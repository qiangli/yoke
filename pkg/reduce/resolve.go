// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs installs the out-handle resolver into the uniform-ref
// registry, over store. The id is the handle verbatim — a bare hex prefix, a
// full hex digest, or a "sha256:"-prefixed digest — exactly as `bashy out`
// accepts it. An ambiguous prefix is a read error naming the collision (the
// caller lengthens it), mirroring `out`; a prefix that matches nothing in a
// readable store is ref.ErrNotFound; a store that cannot be read at all is
// its own error, never "not found".
func RegisterRefs(g *ref.Registry, store *Store) {
	g.Register(ref.Out, ref.ResolverFunc(func(id string) (ref.Node, error) {
		return resolveOut(store, id)
	}))
}

func resolveOut(store *Store, id string) (ref.Node, error) {
	id = strings.TrimSpace(id)
	if store == nil || strings.TrimSpace(store.Root()) == "" {
		return ref.Node{}, fmt.Errorf("out: no spill store configured")
	}
	name, err := store.resolve(id)
	if err != nil {
		if errors.Is(err, ErrNoSpill) {
			return ref.Node{}, fmt.Errorf("out: handle %q: %w", id, ref.ErrNotFound)
		}
		return ref.Node{}, err
	}
	var size int64
	if fi, err := os.Stat(filepath.Join(store.Root(), name)); err == nil {
		size = fi.Size()
	}
	n := ref.NewNode(ref.Out, id)
	n.Title = fmt.Sprintf("%d bytes, sha256:%s", size, name)
	n.Status = "spilled"
	n.Where = filepath.Join(store.Root(), name)
	n.Open = "bashy out " + id
	return n, nil
}
