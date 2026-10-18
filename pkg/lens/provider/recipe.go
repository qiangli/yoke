package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/qiangli/yoke/pkg/ref"
)

// Recipe is an in-process command adapter. The command produces canonical
// JSONL rows for List and Edges; callers retain responsibility for access.
type Recipe struct {
	ListJSONL  func(context.Context, ref.Locator) (io.ReadCloser, error)
	StatNode   func(context.Context, ref.Locator) (Node, error)
	OpenStream func(context.Context, ref.Locator) (io.ReadCloser, error)
	EdgesJSONL func(context.Context, ref.Locator) (io.ReadCloser, error)
}

func (r Recipe) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	if r.ListJSONL == nil {
		return nil, ErrUnsupported
	}
	in, err := r.ListJSONL(ctx, loc)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var rows []Node
	dec := json.NewDecoder(in)
	for {
		var n Node
		err := dec.Decode(&n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if n.SchemaVersion != NodeSchema || n.Ref == "" || n.Kind == "" || n.Type == "" || n.Attrs == nil {
			return nil, fmt.Errorf("provider: invalid %s row", NodeSchema)
		}
		rows = append(rows, n)
	}
	return rows, nil
}

func (r Recipe) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if r.StatNode == nil {
		return Node{}, ErrUnsupported
	}
	return r.StatNode(ctx, loc)
}
func (r Recipe) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	if r.OpenStream == nil {
		return nil, ErrUnsupported
	}
	return r.OpenStream(ctx, loc)
}
func (r Recipe) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	if r.EdgesJSONL == nil {
		return nil, ErrUnsupported
	}
	in, err := r.EdgesJSONL(ctx, loc)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var rows []Edge
	dec := json.NewDecoder(in)
	for {
		var e Edge
		err := dec.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if e.SchemaVersion != EdgeSchema || e.Src == "" || e.Rel == "" || e.Dst == "" || e.Attrs == nil {
			return nil, fmt.Errorf("provider: invalid %s row", EdgeSchema)
		}
		rows = append(rows, e)
	}
	return rows, nil
}

// RegisterRecipe admits the two Wave 1 recipe names. Implementations supply
// commands as callbacks; registration never launches a host process itself.
func (r *Registry) RegisterRecipe(name string, recipe Recipe) error {
	if name != "oci" && name != "k8s" {
		return fmt.Errorf("provider: recipe %q is not admitted", name)
	}
	return r.Register(name, recipe)
}
