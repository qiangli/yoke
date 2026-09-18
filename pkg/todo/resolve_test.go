// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/ref"
)

// seedTodo sets up a hermetic host store and files the given items into it,
// returning the store dir the resolver will land on.
func seedTodo(t *testing.T, items ...*issue.Issue) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, err := UserStore("steward")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Created.IsZero() {
			it.Created = time.Now().UTC()
		}
		if _, err := st.Save(it); err != nil {
			t.Fatal(err)
		}
	}
	return st.Dir()
}

func todoRegistry(t *testing.T) *ref.Registry {
	t.Helper()
	g := ref.NewRegistry()
	// forceUser so resolution never depends on the cwd being (or not being) a
	// git repo — the store is the hermetic host store seedTodo set up.
	RegisterRefs(g, "steward", false, true, "", nil)
	return g
}

func TestResolveTodoFoundByFullID(t *testing.T) {
	dir := seedTodo(t, &issue.Issue{ID: "a1b2c3d4e5f6", Title: "alpha", Status: StatusDoing})
	g := todoRegistry(t)

	n, err := g.Resolve("todo:a1b2c3d4e5f6")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.Kind != ref.Todo || n.ID != "a1b2c3d4e5f6" || n.Ref != "todo:a1b2c3d4e5f6" {
		t.Fatalf("identity = %+v", n)
	}
	if n.Title != "alpha" || n.Status != StatusDoing {
		t.Errorf("title/status = %q/%q", n.Title, n.Status)
	}
	if n.Where != dir {
		t.Errorf("where = %q, want %q", n.Where, dir)
	}
	if n.Open != "bashy todo show a1b2c3d4e5f6" {
		t.Errorf("open = %q", n.Open)
	}
}

// Edge: a unique prefix resolves, and the Node carries the FULL id.
func TestResolveTodoPrefixResolvesToFullID(t *testing.T) {
	seedTodo(t, &issue.Issue{ID: "9988776655ff", Title: "charlie", Status: StatusBlocked})
	g := todoRegistry(t)

	n, err := g.Resolve("todo:9988")
	if err != nil {
		t.Fatalf("prefix should resolve: %v", err)
	}
	if n.ID != "9988776655ff" {
		t.Fatalf("id = %q, want the full id even though a prefix was given", n.ID)
	}
}

func TestResolveTodoNotFound(t *testing.T) {
	seedTodo(t, &issue.Issue{ID: "a1b2c3d4e5f6", Title: "alpha", Status: StatusDoing})
	g := todoRegistry(t)

	_, err := g.Resolve("todo:deadbeef")
	if !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Edge: an ambiguous prefix is an error that NAMES the candidates — never
// ErrNotFound, because the item is not absent, the query is under-specified.
func TestResolveTodoAmbiguousPrefixErrors(t *testing.T) {
	seedTodo(t,
		&issue.Issue{ID: "a1b2c3d4e5f6", Title: "alpha", Status: StatusDoing},
		&issue.Issue{ID: "a1ffffffffff", Title: "bravo", Status: StatusTodo},
	)
	g := todoRegistry(t)

	_, err := g.Resolve("todo:a1")
	if err == nil {
		t.Fatal("ambiguous prefix resolved to one item — silently picking is how the wrong item gets acted on")
	}
	if errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("ambiguous must not be ErrNotFound: %v", err)
	}
	for _, id := range []string{"a1b2c3d4e5f6", "a1ffffffffff"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("ambiguity error does not name candidate %s: %v", id, err)
		}
	}
}
