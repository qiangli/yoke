package lenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/skills"
	"github.com/qiangli/yoke/pkg/spacegraph"
)

func init() {
	RegisterProvider("sphere", func(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return sphereRecipe(spacegraph.Open(skills.DefaultStoreDir()), time.Now), nil
	})
}

func sphereRef(id string) string {
	return (ref.Locator{Scheme: "sphere", Target: id}).String()
}

func sphereRecipe(store *spacegraph.Store, now func() time.Time) provider.Recipe {
	valid := func(loc ref.Locator) error {
		if loc.Scheme != "sphere" || loc.ScopeKind != "" || len(loc.Inner) != 0 {
			return provider.ErrUnsupported
		}
		return nil
	}
	nodes := func(ctx context.Context) ([]spacegraph.Node, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return store.Nodes(now().UTC())
	}
	nodeRow := func(n spacegraph.Node) provider.Node {
		return provider.Node{
			SchemaVersion: provider.NodeSchema,
			Ref:           sphereRef(n.ID),
			Path:          n.ID,
			Name:          n.Name,
			Kind:          string(n.Kind),
			Type:          "sphere/entity",
			Attrs: map[string]any{
				"out": n.Out, "in": n.In,
				"first": n.First.UTC().Format(time.RFC3339Nano),
				"last":  n.Last.UTC().Format(time.RFC3339Nano),
			},
		}
	}
	encode := func(rows any) (io.ReadCloser, error) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		switch xs := rows.(type) {
		case []provider.Node:
			for _, row := range xs {
				if err := enc.Encode(row); err != nil {
					return nil, err
				}
			}
		case []provider.Edge:
			for _, row := range xs {
				if err := enc.Encode(row); err != nil {
					return nil, err
				}
			}
		}
		return io.NopCloser(bytes.NewReader(b.Bytes())), nil
	}
	return provider.Recipe{
		StatNode: func(ctx context.Context, loc ref.Locator) (provider.Node, error) {
			if err := valid(loc); err != nil {
				return provider.Node{}, err
			}
			all, err := nodes(ctx)
			if err != nil {
				return provider.Node{}, err
			}
			if loc.Target == "" || loc.Target == "." {
				n := uint64(len(all))
				return provider.Node{
					SchemaVersion: provider.NodeSchema,
					Ref:           loc.String(), Name: "sphere", Kind: "directory",
					Type: "sphere/graph", Children: &n,
					Attrs: map[string]any{"malformed": store.Malformed()},
				}, nil
			}
			for _, n := range all {
				if n.ID == loc.Target {
					return nodeRow(n), nil
				}
			}
			return provider.Node{}, os.ErrNotExist
		},
		ListJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			if err := valid(loc); err != nil {
				return nil, err
			}
			if loc.Target != "" && loc.Target != "." {
				all, err := nodes(ctx)
				if err != nil {
					return nil, err
				}
				found := false
				for _, n := range all {
					found = found || n.ID == loc.Target
				}
				if !found {
					return nil, os.ErrNotExist
				}
				return encode([]provider.Node{})
			}
			all, err := nodes(ctx)
			if err != nil {
				return nil, err
			}
			rows := make([]provider.Node, 0, len(all))
			for _, n := range all {
				rows = append(rows, nodeRow(n))
			}
			return encode(rows)
		},
		EdgesJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			if err := valid(loc); err != nil {
				return nil, err
			}
			if loc.Target != "" && loc.Target != "." {
				return encode([]provider.Edge{})
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			all, err := store.Edges(now().UTC())
			if err != nil {
				return nil, err
			}
			rows := make([]provider.Edge, 0, len(all))
			for _, e := range all {
				rows = append(rows, provider.Edge{
					SchemaVersion: provider.EdgeSchema,
					Src:           sphereRef(e.Src), Rel: string(e.Rel), Dst: sphereRef(e.Dst),
					Attrs: map[string]any{
						"via": e.Via, "observations": e.N, "successes": e.OK,
						"last":       e.Last.UTC().Format(time.RFC3339Nano),
						"coordinate": e.Coordinate, "place": e.Place, "source": e.Source,
					},
				})
			}
			return encode(rows)
		},
	}
}
