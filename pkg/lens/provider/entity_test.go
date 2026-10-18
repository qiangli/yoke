package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// entityTestRegistry wires one sprint resolver with found / not-found /
// read-error answers and a generic resolver for the other seven ref-kind
// schemes, so every scheme's dispatch is exercised without importing stores.
func entityTestRegistry() *ref.Registry {
	g := ref.NewRegistry()
	g.Register(ref.Sprint, ref.ResolverFunc(func(id string) (ref.Node, error) {
		switch id {
		case "446":
			n := ref.NewNode(ref.Sprint, "446")
			n.Title = "Agent-friendly common options + lens + adapt"
			n.Status = "doing"
			n.Where = "/tmp/sprints"
			n.Open = "bashy sprint show 446"
			n.UID = "0193abcd-0000-4000-8000-000000000000"
			n.Seq = 446
			return n, nil
		case "boom":
			return ref.Node{}, errors.New("weave: read sprint store /tmp/sprints: permission denied")
		default:
			return ref.Node{}, fmt.Errorf("sprint:%s: %w", id, ref.ErrNotFound)
		}
	}))
	for _, k := range []ref.Kind{ref.Run, ref.Agent, ref.MB, ref.Meet, ref.Bus, ref.KB, ref.Host} {
		k := k
		g.Register(k, ref.ResolverFunc(func(id string) (ref.Node, error) {
			if id == "missing" {
				return ref.Node{}, fmt.Errorf("%s:%s: %w", k, id, ref.ErrNotFound)
			}
			n := ref.NewNode(k, id)
			n.Title = string(k) + " " + id
			n.Status = "live"
			return n, nil
		}))
	}
	return g
}

func entityLoc(scheme, target string) ref.Locator {
	return ref.Locator{Scheme: scheme, Target: target}
}

func TestEntityStatAllSchemes(t *testing.T) {
	g := entityTestRegistry()
	ctx := context.Background()
	for _, scheme := range EntitySchemes {
		id := "x1"
		if scheme == "sprint" {
			id = "446"
		}
		n, err := NewEntity(g, scheme).Stat(ctx, entityLoc(scheme, id))
		if err != nil {
			t.Fatalf("%s: Stat: %v", scheme, err)
		}
		if n.SchemaVersion != NodeSchema || n.Ref != scheme+":"+id || n.Path != id {
			t.Fatalf("%s: identity %+v", scheme, n)
		}
		if n.Kind != "record" || n.Type != scheme {
			t.Fatalf("%s: class kind=%q type=%q", scheme, n.Kind, n.Type)
		}
		if n.Name == "" || n.Attrs == nil || n.Attrs["kind"] != scheme || n.Attrs["id"] != id {
			t.Fatalf("%s: name/attrs %+v", scheme, n)
		}
		if n.Children != nil {
			t.Fatalf("%s: children must be omitted (unknown), got %v", scheme, *n.Children)
		}
	}
}

func TestEntityStatSprintFields(t *testing.T) {
	n, err := NewEntity(entityTestRegistry(), "sprint").Stat(context.Background(), entityLoc("sprint", "446"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "Agent-friendly common options + lens + adapt" {
		t.Fatalf("name=%q", n.Name)
	}
	for k, want := range map[string]any{
		"title":  "Agent-friendly common options + lens + adapt",
		"status": "doing", "where": "/tmp/sprints", "open": "bashy sprint show 446",
		"uid": "0193abcd-0000-4000-8000-000000000000", "seq": int64(446),
	} {
		if n.Attrs[k] != want {
			t.Fatalf("attrs[%q]=%v want %v", k, n.Attrs[k], want)
		}
	}
}

func TestEntityNotFoundStaysNotFound(t *testing.T) {
	g := entityTestRegistry()
	for _, tc := range [][2]string{{"sprint", "999"}, {"run", "missing"}, {"kb", "missing"}} {
		_, err := NewEntity(g, tc[0]).Stat(context.Background(), entityLoc(tc[0], tc[1]))
		if !errors.Is(err, ref.ErrNotFound) {
			t.Fatalf("%s:%s: err=%v, want ref.ErrNotFound", tc[0], tc[1], err)
		}
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ref.ErrNoResolver) {
			t.Fatalf("%s:%s: not-found misfiled as unsupported/unwired: %v", tc[0], tc[1], err)
		}
	}
}

func TestEntityReadErrorIsNotNotFound(t *testing.T) {
	_, err := NewEntity(entityTestRegistry(), "sprint").Stat(context.Background(), entityLoc("sprint", "boom"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err=%v", err)
	}
	if errors.Is(err, ref.ErrNotFound) || errors.Is(err, ErrUnsupported) || errors.Is(err, ref.ErrNoResolver) {
		t.Fatalf("read error conflated: %v", err)
	}
}

func TestEntityNoResolverIsNotNotFound(t *testing.T) {
	_, err := NewEntity(ref.NewRegistry(), "sprint").Stat(context.Background(), entityLoc("sprint", "446"))
	if !errors.Is(err, ref.ErrNoResolver) {
		t.Fatalf("err=%v, want ref.ErrNoResolver", err)
	}
	if errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("unwired kind misread as empty store: %v", err)
	}
}

func TestEntityNilRegistryIsEmpty(t *testing.T) {
	var g *ref.Registry
	_, err := NewEntity(g, "sprint").Stat(context.Background(), entityLoc("sprint", "446"))
	if !errors.Is(err, ref.ErrNoResolver) {
		t.Fatalf("err=%v, want ref.ErrNoResolver", err)
	}
}

func TestEntityUnsupportedOps(t *testing.T) {
	e := NewEntity(entityTestRegistry(), "sprint")
	loc := entityLoc("sprint", "446")
	if _, err := e.Open(context.Background(), loc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open err=%v", err)
	}
	if _, err := e.Edges(context.Background(), loc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Edges err=%v", err)
	}
}

func TestEntityList(t *testing.T) {
	e := NewEntity(entityTestRegistry(), "sprint")
	rows, err := e.List(context.Background(), entityLoc("sprint", "446"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if _, err := e.List(context.Background(), entityLoc("sprint", "999")); !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("missing List err=%v", err)
	}
}

func TestEntityUnwiredSchemesReportSeam(t *testing.T) {
	for _, scheme := range UnwiredSchemes {
		e := NewEntity(entityTestRegistry(), scheme)
		loc := entityLoc(scheme, "anything")
		for name, fn := range map[string]func() error{
			"Stat":  func() error { _, err := e.Stat(context.Background(), loc); return err },
			"List":  func() error { _, err := e.List(context.Background(), loc); return err },
			"Open":  func() error { _, err := e.Open(context.Background(), loc); return err },
			"Edges": func() error { _, err := e.Edges(context.Background(), loc); return err },
		} {
			err := fn()
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("%s %s: err=%v, want ErrUnsupported", scheme, name, err)
			}
			if !strings.Contains(err.Error(), "not a ref kind") {
				t.Fatalf("%s %s: seam not named: %v", scheme, name, err)
			}
			if errors.Is(err, ref.ErrNotFound) {
				t.Fatalf("%s %s: seam misread as not found: %v", scheme, name, err)
			}
		}
	}
}

func TestEntityRejectsInnerAndScope(t *testing.T) {
	e := NewEntity(entityTestRegistry(), "sprint")
	inner := entityLoc("sprint", "446")
	inner.Inner = []string{"notes"}
	if _, err := e.Stat(context.Background(), inner); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("inner err=%v", err)
	}
	scoped := entityLoc("sprint", "446")
	scoped.ScopeKind, scoped.ScopeValue = "host", "peer-a"
	if _, err := e.Stat(context.Background(), scoped); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("scope err=%v", err)
	}
	if _, err := e.Stat(context.Background(), entityLoc("sprint", "")); !errors.Is(err, ref.ErrEmptyID) {
		t.Fatalf("empty id err=%v", err)
	}
}

func TestEntityRowJSON(t *testing.T) {
	n, err := NewEntity(entityTestRegistry(), "run").Stat(context.Background(), entityLoc("run", "r1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != NodeSchema || m["ref"] != "run:r1" || m["attrs"] == nil {
		t.Fatalf("row=%s", b)
	}
	if _, ok := m["children"]; ok {
		t.Fatalf("children must be omitted: %s", b)
	}
}

func TestAgentWorldSchemeSets(t *testing.T) {
	if len(EntitySchemes) != 8 || len(UnwiredSchemes) != 2 || len(AgentWorldSchemes) != 10 {
		t.Fatalf("sets=%d/%d/%d", len(EntitySchemes), len(UnwiredSchemes), len(AgentWorldSchemes))
	}
	seen := map[string]bool{}
	for _, s := range AgentWorldSchemes {
		if seen[s] {
			t.Fatalf("duplicate %q", s)
		}
		seen[s] = true
	}
	for _, s := range append(append([]string{}, EntitySchemes...), UnwiredSchemes...) {
		if !seen[s] {
			t.Fatalf("registration set drops %q", s)
		}
		if ref.Known(ref.Kind(s)) != (s != "story" && s != "claim") {
			t.Fatalf("vocabulary drift for %q", s)
		}
	}
}
