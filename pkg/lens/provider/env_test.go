package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

func newTestEnvProvider() *Env {
	fakeEnv := []string{
		"USER=agent",
		"PATH=/usr/bin:/bin",
		"SECRET_TOKEN=supersecret12345",
		"API_KEY=shhh-do-not-leak",
		"EMPTY_VAR=",
		"FOO=bar=baz",
		"A_FIRST=1",
		"Z_LAST=99",
	}
	return NewEnvWithEnviron(fakeEnv)
}

func TestEnvProviderOperations(t *testing.T) {
	ctx := context.Background()
	p := newTestEnvProvider()

	// 1. Stat root
	rootLoc := ref.Locator{Scheme: "env", Target: ""}
	rootNode, err := p.Stat(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if rootNode.SchemaVersion != NodeSchema || rootNode.Ref != "env:" || rootNode.Kind != "directory" || rootNode.Type != "env/tree" {
		t.Errorf("unexpected root node: %+v", rootNode)
	}
	if rootNode.Children == nil || *rootNode.Children != 8 {
		t.Errorf("rootNode.Children = %v, want 8", rootNode.Children)
	}

	// Stat root with target "."
	dotLoc := ref.Locator{Scheme: "env", Target: "."}
	dotNode, err := p.Stat(ctx, dotLoc)
	if err != nil {
		t.Fatalf("Stat dot: %v", err)
	}
	if dotNode.Ref != "env:" {
		t.Errorf("dotNode.Ref = %q, want env:", dotNode.Ref)
	}

	// 2. List root (deterministic sorting)
	vars, err := p.List(ctx, rootLoc)
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if len(vars) != 8 {
		t.Fatalf("len(vars) = %d, want 8", len(vars))
	}
	expectedOrder := []string{"API_KEY", "A_FIRST", "EMPTY_VAR", "FOO", "PATH", "SECRET_TOKEN", "USER", "Z_LAST"}
	for i, wantName := range expectedOrder {
		if vars[i].Name != wantName {
			t.Errorf("vars[%d].Name = %q, want %q", i, vars[i].Name, wantName)
		}
		if vars[i].Ref != "env:"+wantName {
			t.Errorf("vars[%d].Ref = %q, want %q", i, vars[i].Ref, "env:"+wantName)
		}
		if vars[i].Kind != "variable" || vars[i].Type != "variable" {
			t.Errorf("vars[%d] kind/type = %s/%s, want variable/variable", i, vars[i].Kind, vars[i].Type)
		}
		if vars[i].Attrs["present"] != true || vars[i].Attrs["name"] != wantName {
			t.Errorf("vars[%d].Attrs = %+v", i, vars[i].Attrs)
		}
	}

	// Verify secrets NEVER leak into any node field or serialized representation
	for _, v := range vars {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal node %s: %v", v.Name, err)
		}
		s := string(data)
		if strings.Contains(s, "supersecret12345") || strings.Contains(s, "shhh-do-not-leak") {
			t.Fatalf("SECRET LEAKED in node %s: %s", v.Name, s)
		}
	}

	// 3. Stat specific variable
	secretLoc := ref.Locator{Scheme: "env", Target: "SECRET_TOKEN"}
	secretNode, err := p.Stat(ctx, secretLoc)
	if err != nil {
		t.Fatalf("Stat SECRET_TOKEN: %v", err)
	}
	if secretNode.Name != "SECRET_TOKEN" || secretNode.Kind != "variable" {
		t.Errorf("unexpected secret node: %+v", secretNode)
	}
	if secretNode.Attrs["present"] != true {
		t.Errorf("secretNode.Attrs[present] != true")
	}
	secData, _ := json.Marshal(secretNode)
	if strings.Contains(string(secData), "supersecret12345") {
		t.Fatalf("secret leaked in Stat: %s", secData)
	}

	// 4. List specific variable (leaf returns empty slice)
	leafChildren, err := p.List(ctx, secretLoc)
	if err != nil {
		t.Fatalf("List leaf: %v", err)
	}
	if len(leafChildren) != 0 {
		t.Errorf("leaf children len = %d, want 0", len(leafChildren))
	}

	// 5. Edges from root
	edges, err := p.Edges(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Edges root: %v", err)
	}
	if len(edges) != 8 {
		t.Fatalf("len(edges) = %d, want 8", len(edges))
	}
	for i, wantName := range expectedOrder {
		if edges[i].SchemaVersion != EdgeSchema || edges[i].Src != "env:" || edges[i].Rel != "contains" || edges[i].Dst != "env:"+wantName {
			t.Errorf("edges[%d] = %+v, want dst env:%s", i, edges[i], wantName)
		}
	}

	// Edges from leaf
	leafEdges, err := p.Edges(ctx, secretLoc)
	if err != nil {
		t.Fatalf("Edges leaf: %v", err)
	}
	if len(leafEdges) != 0 {
		t.Errorf("leaf edges len = %d, want 0", len(leafEdges))
	}

	// 6. Open returns ErrUnsupported
	_, err = p.Open(ctx, rootLoc)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open root err = %v, want ErrUnsupported", err)
	}
	_, err = p.Open(ctx, secretLoc)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open leaf err = %v, want ErrUnsupported", err)
	}
}

func TestEnvErrorsAndUnsupported(t *testing.T) {
	ctx := context.Background()
	p := newTestEnvProvider()

	// Non-existent variable
	missingLoc := ref.Locator{Scheme: "env", Target: "NONEXISTENT_VAR_12345"}
	if _, err := p.Stat(ctx, missingLoc); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat missing err = %v, want os.ErrNotExist", err)
	}
	if _, err := p.List(ctx, missingLoc); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("List missing err = %v, want os.ErrNotExist", err)
	}
	if _, err := p.Edges(ctx, missingLoc); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Edges missing err = %v, want os.ErrNotExist", err)
	}

	// Wrong scheme
	wrongScheme := ref.Locator{Scheme: "proc", Target: "PATH"}
	if _, err := p.Stat(ctx, wrongScheme); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat wrong scheme err = %v, want ErrUnsupported", err)
	}

	// Subpath / slash in target is unsupported
	slashLoc := ref.Locator{Scheme: "env", Target: "PATH/sub"}
	if _, err := p.Stat(ctx, slashLoc); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat slash target err = %v, want ErrUnsupported", err)
	}

	// Inner descent is unsupported
	innerLoc := ref.Locator{Scheme: "env", Target: "PATH", Inner: []string{"sub"}}
	if _, err := p.Stat(ctx, innerLoc); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat inner err = %v, want ErrUnsupported", err)
	}

	// Scope is unsupported
	scopedLoc := ref.Locator{Scheme: "env", Target: "PATH", ScopeKind: "host", ScopeValue: "remote"}
	if _, err := p.Stat(ctx, scopedLoc); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat scoped err = %v, want ErrUnsupported", err)
	}
}

func TestEnvRegistryAndSerialization(t *testing.T) {
	r := NewRegistry()
	p := newTestEnvProvider()
	if err := r.Register("env", p); err != nil {
		t.Fatalf("Register env: %v", err)
	}

	lookup, ok := r.Lookup("env")
	if !ok || lookup == nil {
		t.Fatal("Lookup env failed")
	}

	loc, err := r.Parse("env:USER")
	if err != nil {
		t.Fatalf("Parse env:USER: %v", err)
	}

	node, err := lookup.Stat(context.Background(), loc)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	b, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["schema_version"] != NodeSchema || m["ref"] != "env:USER" || m["attrs"] == nil {
		t.Fatalf("serialized row invalid: %s", b)
	}
}

func TestEnvLiveHost(t *testing.T) {
	p := NewEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rootLoc := ref.Locator{Scheme: "env", Target: ""}
	vars, err := p.List(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Live host List env: %v", err)
	}
	if len(vars) == 0 {
		t.Fatal("Live host returned 0 environment variables")
	}

	statNode, err := p.Stat(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Live host Stat env: %v", err)
	}
	if statNode.Children == nil || *statNode.Children != uint64(len(vars)) {
		t.Errorf("statNode.Children = %v, len(vars) = %d", statNode.Children, len(vars))
	}

	// First variable should be stat-able
	firstVar := vars[0]
	varLoc := ref.Locator{Scheme: "env", Target: firstVar.Name}
	varNode, err := p.Stat(ctx, varLoc)
	if err != nil {
		t.Fatalf("Live host Stat %s: %v", firstVar.Name, err)
	}
	if varNode.Kind != "variable" {
		t.Errorf("varNode.Kind = %s, want variable", varNode.Kind)
	}

	// Open returns ErrUnsupported
	if _, err := p.Open(ctx, varLoc); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open on live host err = %v, want ErrUnsupported", err)
	}
}
