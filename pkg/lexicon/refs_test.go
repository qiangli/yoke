// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package lexicon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// fakeRegistry swaps RefResolvers for the test's own and restores it after.
// The fakes are the whole point: pkg/lexicon imports no store, so the only
// resolvers a test here can see are the ones it registers.
func fakeRegistry(t *testing.T) *ref.Registry {
	t.Helper()
	prev := RefResolvers
	g := ref.NewRegistry()
	RefResolvers = g
	t.Cleanup(func() { RefResolvers = prev })
	return g
}

// kbFake answers for one kb slug, files everything else as not found, and
// carries its listing verb on the not-found answer the way a store may.
func kbFake(calls *int) ref.ResolverFunc {
	return func(id string) (ref.Node, error) {
		if calls != nil {
			*calls++
		}
		if id != "deploy-runbook" {
			n := ref.NewNode(ref.KB, id)
			n.Open = "bashy kb show " + id
			return n, fmt.Errorf("kb %q: %w", id, ref.ErrNotFound)
		}
		n := ref.NewNode(ref.KB, id)
		n.Title = "Deploy runbook"
		n.Status = "validated"
		n.Where = "kb/pages/deploy-runbook.md"
		n.Open = "bashy kb show deploy-runbook"
		n.Successor = "kb:deploy-runbook-v2"
		return n, nil
	}
}

func runDefine(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewDefineCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestDefineCmd_RefFound(t *testing.T) {
	fakeRegistry(t).Register(ref.KB, kbFake(nil))
	out, err := runDefine(t, "kb:deploy-runbook")
	if err != nil {
		t.Fatalf("define kb:deploy-runbook: %v\n%s", err, out)
	}
	for _, want := range []string{
		"kb:deploy-runbook  Deploy runbook\n",
		"  status: validated\n",
		"  where:  kb/pages/deploy-runbook.md\n",
		"  open:   bashy kb show deploy-runbook\n",
		"  successor: kb:deploy-runbook-v2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unknown here") {
		t.Errorf("a resolved ref fell through to the term path:\n%s", out)
	}
}

func TestDefineCmd_RefFoundJSON(t *testing.T) {
	fakeRegistry(t).Register(ref.KB, kbFake(nil))
	out, err := runDefine(t, "kb:deploy-runbook", "--json")
	if err != nil {
		t.Fatalf("define --json: %v\n%s", err, out)
	}
	var n ref.Node
	if err := json.Unmarshal([]byte(out), &n); err != nil {
		t.Fatalf("--json is not a ref.Node: %v\n%s", err, out)
	}
	if n.Ref != "kb:deploy-runbook" || n.Kind != ref.KB || n.Title != "Deploy runbook" || n.Successor != "kb:deploy-runbook-v2" {
		t.Errorf("node = %+v", n)
	}
}

// Not found is exit 1, names the kind and id, and points at the store's
// listing verb — the Open verb minus the id.
func TestDefineCmd_RefNotFound(t *testing.T) {
	fakeRegistry(t).Register(ref.KB, kbFake(nil))
	out, err := runDefine(t, "kb:nope")
	if err == nil {
		t.Fatalf("define kb:nope succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "kb:nope: no kb with id nope here") {
		t.Errorf("error = %q", err)
	}
	if !strings.Contains(err.Error(), "`bashy kb show`") {
		t.Errorf("no listing hint in %q", err)
	}
}

// A resolver that returns a bare ErrNotFound still gets a way out, without
// this package inventing a verb.
func TestDefineCmd_RefNotFoundWithoutOpen(t *testing.T) {
	fakeRegistry(t).Register(ref.Todo, ref.ResolverFunc(func(id string) (ref.Node, error) {
		return ref.Node{}, ref.ErrNotFound
	}))
	_, err := runDefine(t, "todo:#a5f5")
	if err == nil || !strings.Contains(err.Error(), "todo:a5f5: no todo with id a5f5 here") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "--list-kinds") {
		t.Errorf("no way out of not-found in %q", err)
	}
}

// The nil-hook state: a kind in the vocabulary that this build never wired.
// It must never read as "unknown" or as an empty store.
func TestDefineCmd_RefKindNotWired(t *testing.T) {
	fakeRegistry(t) // empty
	out, err := runDefine(t, "todo:a5f5c1d2e3b4")
	if err == nil {
		t.Fatalf("define on an unwired kind succeeded:\n%s", out)
	}
	if err.Error() != "kind todo: no resolver wired on this build" {
		t.Errorf("error = %q", err)
	}
	if strings.Contains(err.Error(), "not found") || strings.Contains(out, "unknown here") {
		t.Errorf("unwired kind reported as absent:\n%v\n%s", err, out)
	}
}

// A store that could not be read is its own error, verbatim — never "not
// found", never "unknown".
func TestDefineCmd_RefStoreError(t *testing.T) {
	fakeRegistry(t).Register(ref.KB, ref.ResolverFunc(func(id string) (ref.Node, error) {
		return ref.Node{}, errors.New("kb: open pages: permission denied")
	}))
	out, err := runDefine(t, "kb:anything")
	if err == nil || err.Error() != "kb: open pages: permission denied" {
		t.Fatalf("error = %v\n%s", err, out)
	}
}

// urn:dhnt:<x>: and dhnt:<x>/ CLAIM a ref, so a kind outside the vocabulary is
// reported with the vocabulary rather than treated as a word.
func TestDefineCmd_RefUnknownKind(t *testing.T) {
	fakeRegistry(t)
	for _, term := range []string{"urn:dhnt:page:deploy-runbook", "dhnt:page/deploy-runbook@steward"} {
		out, err := runDefine(t, term)
		if err == nil {
			t.Fatalf("define %s succeeded:\n%s", term, out)
		}
		want := term + ": page is not a kind; kinds: " + strings.Join(ref.KindNames(), " ")
		if err.Error() != want {
			t.Errorf("define %s:\n got %q\nwant %q", term, err, want)
		}
	}
}

func TestDefineCmd_RefEmptyID(t *testing.T) {
	fakeRegistry(t)
	out, err := runDefine(t, "kb:")
	if err == nil {
		t.Fatalf("define kb: succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "kb:") || !strings.Contains(err.Error(), "<kind>:<id>") {
		t.Errorf("error = %q", err)
	}
}

// A bare word and a tool:model binding are NOT refs: they take the existing
// term path, exit 0, and never reach a resolver.
func TestDefineCmd_NonRefsTakeTheTermPath(t *testing.T) {
	calls := 0
	g := fakeRegistry(t)
	g.Register(ref.KB, kbFake(&calls))
	g.Register(ref.Tool, ref.ResolverFunc(func(id string) (ref.Node, error) {
		calls++
		return ref.NewNode(ref.Tool, id), nil
	}))
	for _, term := range []string{"zzqxv-no-such-word", "codex:gpt5.6-sol", "note: remember this"} {
		out, err := runDefine(t, term)
		if err != nil {
			t.Fatalf("define %q: %v\n%s", term, err, out)
		}
		if !strings.Contains(out, "unknown here") {
			t.Errorf("define %q did not take the term path:\n%s", term, out)
		}
	}
	if calls != 0 {
		t.Errorf("a non-ref reached a resolver %d time(s)", calls)
	}
}

// The fully-qualified spelling resolves for EVERY kind in the vocabulary, and
// prints the canonical ref — nothing emits urn:dhnt: by default.
func TestDefineCmd_URNSpellingEveryKind(t *testing.T) {
	g := fakeRegistry(t)
	for _, k := range ref.Kinds() {
		k := k
		g.Register(k, ref.ResolverFunc(func(id string) (ref.Node, error) {
			n := ref.NewNode(k, id)
			n.Title = "a " + string(k)
			return n, nil
		}))
	}
	if missing := g.Missing(); len(missing) != 0 {
		t.Fatalf("fake registry misses %v", missing)
	}
	for _, k := range ref.Kinds() {
		term := ref.URN(k, "x1")
		out, err := runDefine(t, term)
		if err != nil {
			t.Fatalf("define %s: %v\n%s", term, err, out)
		}
		if want := ref.Format(k, "x1") + "  a " + string(k) + "\n"; !strings.HasPrefix(out, want) {
			t.Errorf("define %s printed:\n%s\nwant prefix %q", term, out, want)
		}
	}
}

// --list-kinds carries the ref vocabulary as a second group, after the
// projected namespaces.
func TestDefineCmd_ListKindsIncludesRefs(t *testing.T) {
	out, err := runDefine(t, "--list-kinds")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(out, "\nrefs:\n")
	if i < 0 {
		t.Fatalf("no refs: group:\n%s", out)
	}
	if !strings.Contains(out[:i], string(KindStandardTool)) {
		t.Errorf("the projected namespaces do not precede refs::\n%s", out)
	}
	for _, k := range ref.KindNames() {
		if !strings.Contains(out[i:], "\n  "+k+"\n") {
			t.Errorf("refs: group lacks %s:\n%s", k, out[i:])
		}
	}
}
