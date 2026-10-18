package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

const recipeOutputLimit = 4 << 20

type recipeRunner func(context.Context, string, ...string) ([]byte, error)

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("recipe output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

func runRecipe(ctx context.Context, command string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	out := &boundedOutput{limit: recipeOutputLimit}
	errOut := &boundedOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", command, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

type platformRecord struct {
	id, name, kind, typ string
	attrs               map[string]any
}

func recipeNode(scheme, collection string, r platformRecord) Node {
	target := collection + "/" + r.id
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           (ref.Locator{Scheme: scheme, Target: target}).String(),
		Path:          target,
		Name:          r.name,
		Kind:          r.kind,
		Type:          r.typ,
		Attrs:         r.attrs,
	}
}

func recipeRoot(loc ref.Locator, scheme, collection string, count int) Node {
	n := uint64(count)
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          collection,
		Name:          collection,
		Kind:          "directory",
		Type:          scheme + "/collection",
		Children:      &n,
		Attrs:         map[string]any{},
	}
}

func recipeRows(rows any) (io.ReadCloser, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	switch v := rows.(type) {
	case []Node:
		for _, row := range v {
			if err := enc.Encode(row); err != nil {
				return nil, err
			}
		}
	case []Edge:
		for _, row := range v {
			if err := enc.Encode(row); err != nil {
				return nil, err
			}
		}
	default:
		return nil, ErrUnsupported
	}
	return io.NopCloser(bytes.NewReader(b.Bytes())), nil
}

func platformRecipe(scheme, collection string, snapshot func(context.Context) ([]platformRecord, error)) Recipe {
	valid := func(loc ref.Locator) (string, error) {
		if loc.Scheme != scheme || loc.ScopeKind != "" || len(loc.Inner) != 0 {
			return "", ErrUnsupported
		}
		switch {
		case loc.Target == "", loc.Target == collection:
			return "", nil
		case strings.HasPrefix(loc.Target, collection+"/"):
			id := strings.TrimPrefix(loc.Target, collection+"/")
			if id != "" && !strings.Contains(id, "..") {
				return id, nil
			}
		}
		return "", os.ErrNotExist
	}
	nodes := func(ctx context.Context, loc ref.Locator) ([]Node, error) {
		id, err := valid(loc)
		if err != nil {
			return nil, err
		}
		if id != "" {
			return []Node{}, nil
		}
		records, err := snapshot(ctx)
		if err != nil {
			return nil, err
		}
		rows := make([]Node, 0, len(records))
		for _, record := range records {
			rows = append(rows, recipeNode(scheme, collection, record))
		}
		return rows, nil
	}
	return Recipe{
		StatNode: func(ctx context.Context, loc ref.Locator) (Node, error) {
			id, err := valid(loc)
			if err != nil {
				return Node{}, err
			}
			records, err := snapshot(ctx)
			if err != nil {
				return Node{}, err
			}
			if id == "" {
				return recipeRoot(loc, scheme, collection, len(records)), nil
			}
			for _, record := range records {
				if record.id == id {
					return recipeNode(scheme, collection, record), nil
				}
			}
			return Node{}, os.ErrNotExist
		},
		ListJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			rows, err := nodes(ctx, loc)
			if err != nil {
				return nil, err
			}
			return recipeRows(rows)
		},
		EdgesJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			rows, err := nodes(ctx, loc)
			if err != nil {
				return nil, err
			}
			edges := make([]Edge, 0, len(rows))
			for _, row := range rows {
				edges = append(edges, Edge{SchemaVersion: EdgeSchema, Src: loc.String(), Rel: "contains", Dst: row.Ref, Attrs: map[string]any{}})
			}
			return recipeRows(edges)
		},
	}
}

func jsonString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		if len(x) > 0 {
			return jsonString(x[0])
		}
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := jsonString(m[key]); s != "" {
			return s
		}
	}
	return ""
}

func parseOCI(data []byte) ([]platformRecord, error) {
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("oci recipe: %w", err)
	}
	rows := make([]platformRecord, 0, len(items))
	for _, item := range items {
		id := firstString(item, "Id", "ID", "id")
		if id == "" || strings.ContainsAny(id, "/!:") {
			return nil, fmt.Errorf("oci recipe: missing or invalid container id")
		}
		name := firstString(item, "Names", "Name", "names")
		if name == "" {
			name = id
		}
		rows = append(rows, platformRecord{id: id, name: name, kind: "container", typ: "oci/container", attrs: map[string]any{
			"image": firstString(item, "Image", "image"),
			"state": firstString(item, "State", "Status", "state"),
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return rows, nil
}

func parseK8sPods(data []byte) ([]platformRecord, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				UID       string `json:"uid"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("k8s recipe: %w", err)
	}
	rows := make([]platformRecord, 0, len(list.Items))
	for _, item := range list.Items {
		ns, name := item.Metadata.Namespace, item.Metadata.Name
		if ns == "" {
			ns = "default"
		}
		if name == "" || strings.ContainsAny(ns+name, "/!:") {
			return nil, fmt.Errorf("k8s recipe: missing or invalid pod identity")
		}
		rows = append(rows, platformRecord{id: ns + "/" + name, name: name, kind: "pod", typ: "k8s/pod", attrs: map[string]any{
			"namespace": ns, "uid": item.Metadata.UID, "phase": item.Status.Phase,
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return rows, nil
}

// NewOCIRecipe lists local containers with a read-only CLI snapshot.
func NewOCIRecipe(run recipeRunner) Recipe {
	if run == nil {
		run = runRecipe
	}
	return platformRecipe("oci", "containers", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "podman", "ps", "--all", "--format", "json")
		if err != nil {
			return nil, err
		}
		return parseOCI(data)
	})
}

// NewK8sRecipe lists pods from the selected kubectl context without mutation.
func NewK8sRecipe(run recipeRunner) Recipe {
	if run == nil {
		run = runRecipe
	}
	return platformRecipe("k8s", "pods", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "kubectl", "get", "pods", "--all-namespaces", "-o", "json")
		if err != nil {
			return nil, err
		}
		return parseK8sPods(data)
	})
}
