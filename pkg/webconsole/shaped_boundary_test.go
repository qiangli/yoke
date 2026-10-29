// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	_ "github.com/qiangli/coreutils/cmds/all"
	"github.com/qiangli/yoke/pkg/shape/boundary"
	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/websession"
)

// boundaryInterpArg as os.Args[1] turns the test binary into the unit
// interpreter: `-c BODY NAME` run in-process under the boundary the console
// handed it in $BASHY_APP_BOUNDARY, with bashy's own pure-Go tools — the
// wiring bashy's interpreter uses, and the same on every OS.
const boundaryInterpArg = "shape-boundary-interp"

func boundaryInterpMain(args []string) int {
	if len(args) < 2 || args[0] != "-c" {
		fmt.Fprintln(os.Stderr, "usage: -c BODY [NAME]")
		return 2
	}
	g, ok, err := boundary.FromEnv(nil)
	if err != nil || !ok {
		fmt.Fprintln(os.Stderr, "unit ran without a boundary:", err)
		return 99
	}
	dir, err := os.Getwd()
	if err != nil {
		return 98
	}
	name := "unit"
	if len(args) > 2 {
		name = args[2]
	}
	code, err := boundary.Run(context.Background(), g, args[1], name, dir, os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return code
}

// commitApp commits files (relative path -> content) as an app's base.
func commitApp(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(name+": base", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.test", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func unitDoc(body string) string { return "# unit\n\n```bsh\n" + body + "\n```\n" }

// TestShapedAppDataBoundary is the A5d proof through the console: every row
// of the operator's denial table is refused by the interpreter, and a
// cross-app call through the owning app's interface, as the same
// principal, succeeds — with no sandbox and nothing POSIX-only.
func TestShapedAppDataBoundary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	q := shellSingleQuote
	todoAlice, _ := shape.OpenAt(home, "todo", "device:alice")
	todoData, err := todoAlice.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := todoAlice.WriteData("items.txt", []byte("milk\n")); err != nil {
		t.Fatal(err)
	}
	notesBob, _ := shape.OpenAt(home, "notes", "device:bob")
	bobData, err := notesBob.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	docs := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	notes := commitApp(t, "notes", map[string]string{
		"public/index.html":   "<h1>Notes</h1>\n",
		"api/own.md":          unitDoc("echo hi > n.txt; cat n.txt"),
		"api/todo.md":         unitDoc("app call todo items || echo none"),
		"api/todo-missing.md": unitDoc("app call todo nope || echo missing"),
		"api/peek-todo.md":    unitDoc("cat " + q(filepath.Join(todoData, "items.txt"))),
		"api/poke-todo.md":    unitDoc("echo x > " + q(filepath.Join(todoData, "items.txt"))),
		"api/poke-bob.md":     unitDoc("echo x > " + q(filepath.Join(bobData, "x"))),
		"api/outside.md":      unitDoc("echo x > " + q(filepath.Join(docs, "x"))),
		"api/home.md":         unitDoc("echo x > " + q(filepath.Join(home, "x"))),
		"api/exec.md":         unitDoc("hostprog --version"),
		"api/widen.md":        unitDoc("@writes(" + q(filepath.ToSlash(docs)) + ")\nsave() { echo d > " + q(filepath.Join(docs, "w.txt")) + "; echo saved; }\nsave"),
	})
	todo := commitApp(t, "todo", map[string]string{
		"public/index.html": "<h1>Todo</h1>\n",
		"api/items.md":      unitDoc("cat items.txt"),
	})
	panels := []Panel{
		{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: notes, Auth: AuthSystem, Source: "registered", Available: true},
		{Name: "todo", Label: "Todo", Path: "/todo/", ShapedSource: todo, Auth: AuthSystem, Source: "registered", Available: true},
	}
	pairPath := filepath.Join(home, "pairing.json")
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing: true, PairStorePath: pairPath, Panels: panels,
		Shape: ShapeOptions{Home: home, Runner: []string{exe, boundaryInterpArg}, Level: contain.LevelProcess},
	})
	pairs := newPairStore(pairPath)
	alice := pairedPrincipal(t, h, pairs, "alice")
	bob := pairedPrincipal(t, h, pairs, "bob")
	get := func(who http.Header, unit string) (int, string) {
		t.Helper()
		w := do(h, http.MethodGet, "/notes/api/"+unit, "192.0.2.20:5555", who)
		return w.Code, w.Body.String()
	}

	for _, tc := range []struct {
		unit string
		code int
		body string
	}{
		{"own", http.StatusOK, "hi\n"},
		{"todo", http.StatusOK, "milk\n"},            // cross-app call, same principal
		{"todo-missing", http.StatusOK, "missing\n"}, // a missing item is a normal result
		{"peek-todo", http.StatusInternalServerError, "(exit 126)"},
		{"poke-todo", http.StatusInternalServerError, "(exit 1)"},
		{"poke-bob", http.StatusInternalServerError, "(exit 1)"},
		{"outside", http.StatusInternalServerError, "(exit 1)"},
		{"home", http.StatusInternalServerError, "(exit 1)"},
		{"exec", http.StatusInternalServerError, "(exit 126)"},
		{"widen", http.StatusForbidden, ""},
	} {
		code, body := get(alice, tc.unit)
		// A refusal's body names the unit's exit: 126 for a refused command,
		// 1 for a refused redirection.
		if code != tc.code || (tc.code == http.StatusOK && body != tc.body) || !strings.Contains(body, tc.body) {
			t.Errorf("alice %s = %d %q, want %d %q", tc.unit, code, body, tc.code, tc.body)
		}
	}
	if code, body := get(alice, "widen"); !strings.Contains(body, "cannot widen") {
		t.Errorf("widen refusal = %d %q, want the widening reason", code, body)
	}
	// Bob's call reaches todo as bob: his (empty) todo data, never alice's.
	if code, body := get(bob, "todo"); code != http.StatusOK || body != "none\n" {
		t.Errorf("bob todo = %d %q, want none", code, body)
	}

	// Granting the declared location at adoption admits it; revoking
	// refuses it again.
	aliceNotes, _ := shape.OpenAt(home, "notes", "device:alice")
	if err := aliceNotes.Write(shape.State{Granted: &shape.Grants{Write: []string{docs}}}); err != nil {
		t.Fatal(err)
	}
	if code, body := get(alice, "widen"); code != http.StatusOK || body != "saved\n" {
		t.Errorf("granted widen = %d %q", code, body)
	}
	if err := aliceNotes.Write(shape.State{}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(alice, "widen"); code != http.StatusForbidden {
		t.Errorf("revoked widen = %d, want 403", code)
	}

	if b, _ := os.ReadFile(filepath.Join(todoData, "items.txt")); string(b) != "milk\n" {
		t.Errorf("another app's data changed: %q", b)
	}
	for _, p := range []string{filepath.Join(bobData, "x"), filepath.Join(docs, "x"), filepath.Join(home, "x")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a denied write landed: %s", p)
		}
	}
}
