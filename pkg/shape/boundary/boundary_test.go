// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package boundary

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/qiangli/coreutils/cmds/all"
)

// world is a bashy home with two apps (notes, todo) and two principals
// (alice, bob), laid out the way pkg/shape/state lays them out, plus a
// directory outside every app (docs).
type world struct {
	home, alice, bob, todo, docs string
}

func newWorld(t *testing.T) world {
	t.Helper()
	root := t.TempDir()
	w := world{
		home:  filepath.Join(root, "bashy"),
		docs:  filepath.Join(root, "Documents"),
		alice: filepath.Join(root, "bashy", "shape", "notes", "alice", "data"),
		bob:   filepath.Join(root, "bashy", "shape", "notes", "bob", "data"),
		todo:  filepath.Join(root, "bashy", "shape", "todo", "alice", "data"),
	}
	for _, d := range []string{w.alice, w.bob, w.todo, w.docs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(w.todo, "items.txt"), "milk\n")
	write(filepath.Join(w.bob, "secret.txt"), "bob's\n")
	write(filepath.Join(w.docs, "r.txt"), "doc\n")
	write(filepath.Join(w.home, "shape", "notes", "alice", "state.json"), "{}\n")
	return w
}

// run executes script in an in-process interpreter carrying g, the way the
// unit runner's interpreter does: boundary outside the pure-Go tools.
func run(t *testing.T, g *Guard, script string) (string, string, int) {
	t.Helper()
	p := g.Policy()
	env := []string{"BASHY_APP_DATA=" + p.Data, "BASHY_APP=" + p.App, "BASHY_PRINCIPAL=" + p.Principal}
	var out, errb bytes.Buffer
	code, err := Run(context.Background(), g, script, "unit", p.Data, env, nil, &out, &errb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out.String(), errb.String(), code
}

// apps is an in-process host: each app's unit runs under ITS OWN guard,
// as the caller's principal; another principal or an unknown unit is a
// normal answer.
func (w world) caller(t *testing.T) Caller {
	return CallerFunc(func(ctx context.Context, req CallRequest) (CallResult, error) {
		if req.App != "todo" || req.Principal != "alice" {
			return CallResult{Status: http.StatusNotFound}, nil
		}
		if req.Unit != "list" {
			return CallResult{Status: http.StatusNotFound}, nil
		}
		g, err := New(Policy{App: "todo", Principal: req.Principal, Data: w.todo, Home: w.home})
		if err != nil {
			return CallResult{}, err
		}
		out, _, code := run(t, g, `cat items.txt`)
		if code != 0 {
			return CallResult{Status: http.StatusInternalServerError}, nil
		}
		return CallResult{Status: http.StatusOK, Body: []byte(out)}, nil
	})
}

func (w world) guard(t *testing.T, grants Grants) *Guard {
	t.Helper()
	g, err := New(Policy{App: "notes", Principal: "alice", Data: w.alice, Home: w.home, Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	return g.WithCaller(w.caller(t))
}

// TestDenialTable is the operator's table, run through the interpreter on
// every OS with nothing but bashy's own tools: each row is denied with a
// reason, except the cross-app call through the owner's interface.
func TestDenialTable(t *testing.T) {
	w := newWorld(t)
	q := func(p string) string { return `'` + p + `'` }
	j := filepath.Join
	cases := []struct {
		name   string
		grants Grants
		script string
		// reason is a substring of the refusal; "" means the run succeeds
		// and its stdout is want.
		reason string
		want   string
	}{
		{name: "write into another principal's data (redirection)", script: "echo x > " + q(j(w.bob, "f")), reason: "another principal's data"},
		{name: "write into another principal's data (tool)", script: "touch " + q(j(w.bob, "f")), reason: "another principal's data"},
		{name: "read another principal's data", script: "cat " + q(j(w.bob, "secret.txt")), reason: "another principal's data"},
		{name: "read another app's data (tool)", script: "cat " + q(j(w.todo, "items.txt")), reason: "another app's data"},
		{name: "read another app's data (redirection)", script: "read l < " + q(j(w.todo, "items.txt")), reason: "another app's data"},
		{name: "read another app's data after cd", script: "cd " + q(w.todo) + " && cat items.txt", reason: "another app's data"},
		{name: "write another app's data", script: "echo x > n.txt; cp n.txt " + q(w.todo), reason: "another app's data"},
		{name: "write outside granted locations", script: "echo x > " + q(j(w.docs, "f")), reason: "outside the app's own data"},
		{name: "read outside granted locations", script: "cat " + q(j(w.docs, "r.txt")), reason: "outside the app's own data"},
		{name: "write beyond a read grant", grants: Grants{Read: []string{w.docs}}, script: "touch " + q(j(w.docs, "f")), reason: "outside the app's own data"},
		{name: "own state is not app data", script: "cat ../state.json", reason: "own state"},
		{name: "write bashy state", script: "echo x > " + q(j(w.home, "x")), reason: "BASHY_HOME"},
		{name: "a grant never reaches bashy state", grants: Grants{Write: []string{w.home}}, script: "touch " + q(j(w.bob, "f")), reason: "another principal's data"},
		{name: "undeclared external program", script: "hostprog --version", reason: "undeclared external program"},
		{name: "undeclared program via an in-process launcher", script: "env hostprog", reason: "undeclared external program"},
		{name: "network redirection", script: "echo x > /dev/tcp/127.0.0.1/80", reason: "network"},

		{name: "own data read and write", script: "echo hi > n.txt; cat n.txt; ls", want: "hi\nn.txt\n"},
		{name: "granted write", grants: Grants{Write: []string{w.docs}}, script: "echo d > " + q(j(w.docs, "out.txt")) + "; cat " + q(j(w.docs, "out.txt")), want: "d\n"},
		{name: "granted read", grants: Grants{Read: []string{w.docs}}, script: "cat " + q(j(w.docs, "r.txt")), want: "doc\n"},
		{name: "dev null", script: "echo x > /dev/null; echo ok", want: "ok\n"},
		{name: "cross-app call through the owner's interface", script: "app call todo list", want: "milk\n"},
		{name: "cross-app call spelled through bashy", script: "bashy app call todo list", want: "milk\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, code := run(t, w.guard(t, tc.grants), tc.script)
			if tc.reason == "" {
				if code != 0 || out != tc.want {
					t.Fatalf("want OK %q, got exit %d out %q stderr %q", tc.want, code, out, stderr)
				}
				return
			}
			if code == 0 || !strings.Contains(stderr, "boundary:") || !strings.Contains(stderr, tc.reason) {
				t.Fatalf("want denied (%s), got exit %d out %q stderr %q", tc.reason, code, out, stderr)
			}
		})
	}
	if _, err := os.Stat(j(w.bob, "f")); err == nil {
		t.Fatal("a denied write landed in another principal's data")
	}
	if _, err := os.Stat(j(w.todo, "n.txt")); err == nil {
		t.Fatal("a denied write landed in another app's data")
	}
}

// TestCandidateCannotWidenGrants: what a unit declares is admitted only
// within what the user granted at adoption.
func TestCandidateCannotWidenGrants(t *testing.T) {
	w := newWorld(t)
	granted := Grants{Read: []string{w.docs}, Programs: []string{"git"}}
	for _, declared := range []Grants{
		{Write: []string{w.docs}},
		{Read: []string{filepath.Dir(w.docs)}},
		{Programs: []string{"curl"}},
	} {
		if _, err := Admit(declared, granted); !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "cannot widen") {
			t.Fatalf("Admit(%+v) = %v, want a widening refusal", declared, err)
		}
	}
	got, err := Admit(Grants{Read: []string{filepath.Join(w.docs, "sub")}, Programs: []string{"git"}}, granted)
	if err != nil || len(got.Read) != 1 || len(got.Write) != 0 {
		t.Fatalf("Admit within the grant = %+v, %v", got, err)
	}
}

// TestCrossAppCallIsANormalResult: a missing unit, another principal's
// app or a run with no interface is exit 1 and a reason — never a crash.
func TestCrossAppCallIsANormalResult(t *testing.T) {
	w := newWorld(t)
	_, stderr, code := run(t, w.guard(t, Grants{}), "app call todo missing || echo handled >&2")
	if code != 0 || !strings.Contains(stderr, "not found") || !strings.Contains(stderr, "handled") {
		t.Fatalf("missing unit: exit %d stderr %q", code, stderr)
	}
	g, _ := New(Policy{App: "notes", Principal: "alice", Data: w.alice, Home: w.home})
	if _, stderr, code := run(t, g, "app call todo list"); code != 1 || !strings.Contains(stderr, "no cross-app interface") {
		t.Fatalf("no endpoint: exit %d stderr %q", code, stderr)
	}
}

func TestPolicyRoundTripsThroughEnv(t *testing.T) {
	w := newWorld(t)
	p := Policy{App: "notes", Principal: "alice", Data: w.alice, Home: w.home, Call: &Endpoint{URL: "http://127.0.0.1:1/call", Token: "t"}}
	v, err := Env(p)
	if err != nil {
		t.Fatal(err)
	}
	g, ok, err := FromEnv([]string{"X=1", EnvVar + "=" + v})
	if err != nil || !ok || g.Policy().Data != w.alice || g.Policy().Call.Token != "t" {
		t.Fatalf("FromEnv = %v %v %v", g, ok, err)
	}
	if _, ok, err := FromEnv([]string{EnvVar + "={"}); !ok || err == nil {
		t.Fatal("a malformed boundary must be an error, never 'no boundary'")
	}
	if _, ok, _ := FromEnv([]string{"X=1"}); ok {
		t.Fatal("no boundary set reported as set")
	}
}

func TestSymlinkOutOfDataIsDenied(t *testing.T) {
	w := newWorld(t)
	if err := os.Symlink(w.todo, filepath.Join(w.alice, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, stderr, code := run(t, w.guard(t, Grants{}), "cat link/items.txt")
	if code == 0 || !strings.Contains(stderr, "another app's data") {
		t.Fatalf("symlink escape: exit %d stderr %q", code, stderr)
	}
}

func TestDeclarationDecoratorsRunThrough(t *testing.T) {
	w := newWorld(t)
	g := w.guard(t, Grants{Write: []string{w.docs}})
	out, stderr, code := run(t, g, "@writes(\""+filepath.ToSlash(w.docs)+"\")\nsave() { echo d > "+`'`+filepath.Join(w.docs, "d.txt")+`'`+"; echo saved; }\nsave\n")
	if code != 0 || out != "saved\n" {
		t.Fatalf("exit %d out %q stderr %q", code, out, stderr)
	}
}

func TestPreviewCopyCannotReachRealData(t *testing.T) {
	w := newWorld(t)
	preview := filepath.Join(filepath.Dir(w.alice), "preview", "c1")
	if err := os.MkdirAll(preview, 0o700); err != nil {
		t.Fatal(err)
	}
	g, _ := New(Policy{App: "notes", Principal: "alice", Data: preview, Home: w.home})
	_, stderr, code := run(t, g, "touch "+`'`+filepath.Join(w.alice, "x")+`'`)
	if code == 0 || !strings.Contains(stderr, "own state") {
		t.Fatalf("preview reached real data: exit %d stderr %q", code, stderr)
	}
}
