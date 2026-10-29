// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/websession"
)

// historyFixture builds a managed src checkout at home/src/notes from the
// notes app: a base release under refs/heads/pristine, then one commit per
// version. Each version appends its own marker comment to public/index.html
// (the frozen smoke check still passes) and declares a data-schema range,
// so previews, diffs and the schema gate have something to chew on.
func historyFixture(t *testing.T, home string) (source string, v1, v2, v3, v4, narrow, vbroken, vb string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if err := copyTree(filepath.Join(notesFixture, "app"), work); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(msg string) string {
		t.Helper()
		wt, err := r.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{
			Name: "history-fixture", Email: "history@example.test", When: time.Now(),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	feature := func(min, max int) string {
		return fmt.Sprintf("schema_version: bashy-shape-feature-v1\nid: notes\norigin: device:alice\nbase_app: notes\nrevision: auto\nschema_range: {min: %d, max: %d}\n", min, max)
	}
	mark := func(tag string) {
		t.Helper()
		p := filepath.Join(work, "public", "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, "<!-- "+tag+" -->\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	version := func(tag, body string, min, max int) string {
		t.Helper()
		write("app.txt", body)
		write("shape.yaml", feature(min, max))
		mark(tag)
		return commit("notes: " + tag)
	}
	base := commit("notes: base release")
	_ = base
	v1 = version("v1", "app-v1\n", 1, 5)
	v2 = version("v2", "app-v2\n", 1, 5)
	v3 = version("v3", "app-v3\n", 1, 5)
	v4 = version("v4", "app-v4\n", 1, 5)
	narrow = version("narrow", "app-narrow\n", 1, 2)
	// vbroken fails the frozen smoke check: its tree contains BROKEN, which
	// the base checks refuse, so a restore of it must be gate-refused.
	write("app.txt", "app-broken\n")
	write("shape.yaml", feature(1, 5))
	mark("vbroken")
	p := filepath.Join(work, "public", "index.html")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, "BROKEN\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	vbroken = commit("notes: vbroken")
	vb = version("vb", "app-bob\n", 1, 5)

	source = filepath.Join(home, "src", "notes")
	if _, err := gogit.PlainClone(source, false, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatal(err)
	}
	sr, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sr.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", plumbing.NewHash(base))); err != nil {
		t.Fatal(err)
	}
	return source, v1, v2, v3, v4, narrow, vbroken, vb
}

type historyTestVersion struct {
	Activation string   `json:"activation"`
	Revision   string   `json:"revision"`
	Kind       string   `json:"kind"`
	Request    string   `json:"request"`
	Verdict    string   `json:"verdict"`
	Evidence   string   `json:"evidence"`
	Principal  string   `json:"principal"`
	Author     string   `json:"author"`
	Names      []string `json:"names"`
	Active     bool     `json:"active"`
}

type historyTestView struct {
	App       string               `json:"app"`
	Principal string               `json:"principal"`
	Tip       string               `json:"tip"`
	Active    string               `json:"active"`
	Versions  []historyTestVersion `json:"versions"`
	Named     []struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
	} `json:"named"`
}

// TestShapedHistoryView is the S525.A10w proof over the real handlers with
// two principals on a temp store: alice activates four versions and the
// history view lists them with who/when/request/verdict/evidence, previews
// one without moving active, restores one back through the evidence gate,
// undoes the restore, names one and diffs two; bob's history and active
// version are unchanged and alice's versions are 404 to him; an
// incompatible and a gate-refused restore leave active unchanged; both data
// directories are untouched; a rejected candidate leaves no workspace; and
// the page controls carry accessible names.
func TestShapedHistoryView(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, v1, v2, v3, v4, narrow, vbroken, vb := historyFixture(t, home)

	for user, body := range map[string]string{"alice": "alice-private\n", "bob": "bob-private\n"} {
		st, err := shape.OpenAt(home, "notes", "device:"+user)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteData("notes.txt", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	dataUntouched := func(stage string) {
		t.Helper()
		for user, want := range map[string]string{"alice": "alice-private\n", "bob": "bob-private\n"} {
			st, err := shape.OpenAt(home, "notes", "device:"+user)
			if err != nil {
				t.Fatal(err)
			}
			b, err := st.ReadData("notes.txt")
			if err != nil || string(b) != want {
				t.Fatalf("%s: device:%s data = %q, want %q", stage, user, b, want)
			}
		}
	}

	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "shape-work"), []string{"/bin/sh"}
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	panel := Panel{Name: "notes", Label: "Notes", Path: "/notes/",
		ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: sessions,
		Pairing: true, PairStorePath: pairPath, Panels: []Panel{panel}, Shape: opts,
	})
	pairs := newPairStore(pairPath)
	alice := pairedPrincipal(t, h, pairs, "alice")
	bob := pairedPrincipal(t, h, pairs, "bob")
	const peer = "192.0.2.20:5555"

	get := func(who http.Header, path string, want int) string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, who)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	postJSON := func(who http.Header, path, body string, want int) string {
		t.Helper()
		hdr := who.Clone()
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set("Content-Type", "application/json")
		w := send(h, http.MethodPost, path, peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	post := func(who http.Header, path string, want int) string {
		t.Helper()
		return postJSON(who, path, "", want)
	}
	activeOf := func(principal string) string {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		return got.ActiveRevision
	}
	historyOf := func(who http.Header) historyTestView {
		t.Helper()
		var v historyTestView
		if err := json.Unmarshal([]byte(get(who, "/api/shape/notes/history", 200)), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	tipOf := func(who http.Header) string { return historyOf(who).Tip }

	setupGate := shape.GateFunc(func(candidate, base, principal string) (shape.EvidenceRef, error) {
		return shape.EvidenceRef{Verdict: "pass", EvidenceID: "ev-setup"}, nil
	})
	activate := func(rev, principal string) {
		t.Helper()
		if _, err := src.Activate(home, "notes", rev, principal, src.ActivateOptions{Gate: setupGate}); err != nil {
			t.Fatalf("activate %s for %s: %v", rev[:12], principal, err)
		}
	}

	// Anonymous callers get the base-app rule: no history verbs at all.
	get(nil, "/api/shape/notes/history", http.StatusForbidden)
	postJSON(nil, "/api/shape/notes/history/restore", `{"rev":"x"}`, http.StatusForbidden)

	// Alice activates four versions; bob activates his own.
	for _, rev := range []string{v1, v2, v3, v4} {
		activate(rev, "device:alice")
	}
	activate(vb, "device:bob")
	if got := activeOf("device:alice"); got != v4 {
		t.Fatalf("alice active = %s, want v4", got)
	}

	// The history lists all four newest-first with who, request text,
	// verifier result, evidence and the active marker.
	ah := historyOf(alice)
	if len(ah.Versions) != 4 {
		t.Fatalf("alice versions = %d, want 4", len(ah.Versions))
	}
	for i, want := range []string{v4, v3, v2, v1} {
		e := ah.Versions[i]
		if e.Revision != want || e.Request != want {
			t.Fatalf("version %d = %+v, want revision+request %s", i, e, want)
		}
		if e.Verdict != "pass" || e.Evidence != "ev-setup" || e.Principal != "device:alice" {
			t.Fatalf("version %d = %+v, want verdict/evidence/principal trailers", i, e)
		}
		if e.Author == "" || e.Activation == "" {
			t.Fatalf("version %d = %+v, want author and activation commit", i, e)
		}
		if want == v4 && !e.Active {
			t.Fatalf("version %d = %+v, want the active marker on v4", i, e)
		}
		if want != v4 && e.Active {
			t.Fatalf("version %d = %+v, active marker on a non-active version", i, e)
		}
	}
	if ah.Active != v4 {
		t.Fatalf("history active = %s, want v4", ah.Active)
	}

	// Bob's history is his own version only.
	bh := historyOf(bob)
	if len(bh.Versions) != 1 || bh.Versions[0].Revision != vb || !bh.Versions[0].Active {
		t.Fatalf("bob history = %+v, want only his active version", bh.Versions)
	}
	if body := get(bob, "/api/shape/notes/history", 200); strings.Contains(body, v2) {
		t.Fatal("bob's history names alice's version")
	}

	// Preview v2: the old tree, served for alice, while her active app and
	// revision stay on v4.
	if body := get(alice, "/shape/notes/history/"+v2+"/", 200); !strings.Contains(body, "<!-- v2 -->") || strings.Contains(body, "<!-- v4 -->") {
		t.Fatalf("history preview does not show the v2 tree: %.200q", body)
	}
	if body := get(alice, "/notes/", 200); !strings.Contains(body, "<!-- v4 -->") {
		t.Fatalf("preview moved alice's active app: %.200q", body)
	}
	if got := activeOf("device:alice"); got != v4 {
		t.Fatalf("preview moved alice active to %s", got)
	}

	// Restore v3 back: a new forward commit through the evidence gate.
	var restored struct {
		Revision string `json:"revision"`
		Previous string `json:"previous"`
		Active   string `json:"active"`
	}
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/restore", `{"rev":`+jsonQuote(v3)+`}`, 200)), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Revision != v3 || restored.Previous != v4 || restored.Active != v3 {
		t.Fatalf("restore = %+v, want revision+active v3 previous v4", restored)
	}
	if got := activeOf("device:alice"); got != v3 {
		t.Fatalf("after restore alice active = %s, want v3", got)
	}
	ah = historyOf(alice)
	if len(ah.Versions) != 5 || ah.Versions[0].Kind != "restore" || ah.Versions[0].Revision != v3 {
		t.Fatalf("after restore newest = %+v, want a restore of v3", ah.Versions[0])
	}
	if ah.Versions[0].Verdict != "pass" || ah.Versions[0].Evidence == "" {
		t.Fatalf("restore entry = %+v, want a passing verdict with sealed evidence", ah.Versions[0])
	}

	// Undo the restore: the previous active version comes back.
	var undone struct {
		Revision string `json:"revision"`
		Active   string `json:"active"`
	}
	if err := json.Unmarshal([]byte(post(alice, "/api/shape/notes/history/undo", 200)), &undone); err != nil {
		t.Fatal(err)
	}
	if undone.Revision != v4 || undone.Active != v4 {
		t.Fatalf("undo = %+v, want revision+active v4", undone)
	}
	if got := activeOf("device:alice"); got != v4 {
		t.Fatalf("after undo alice active = %s, want v4", got)
	}

	// Name v2 and diff v1 against v2.
	var named struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/name", `{"rev":`+jsonQuote(v2)+`,"name":"good"}`, 200)), &named); err != nil {
		t.Fatal(err)
	}
	if named.Name != "good" || named.Revision != v2 {
		t.Fatalf("name = %+v, want good -> v2", named)
	}
	ah = historyOf(alice)
	found := false
	for _, v := range ah.Versions {
		if v.Revision == v2 && len(v.Names) == 1 && v.Names[0] == "good" {
			found = true
		}
	}
	if !found {
		t.Fatalf("v2 carries no name after naming: %+v", ah.Versions)
	}
	diff := get(alice, "/api/shape/notes/history/diff?from="+v1+"&to="+v2, 200)
	if !strings.Contains(diff, `class="add"`) || !strings.Contains(diff, "app-v1") || !strings.Contains(diff, "app-v2") {
		t.Fatalf("diff v1..v2 = %.200q, want both version markers escaped", diff)
	}

	// Bob cannot see alice's versions anywhere: preview, diff, name and
	// restore are all 404 for him, and his own line never moves.
	get(bob, "/shape/notes/history/"+v2+"/", http.StatusNotFound)
	get(bob, "/api/shape/notes/history/diff?from="+v1+"&to="+v2, http.StatusNotFound)
	postJSON(bob, "/api/shape/notes/history/name", `{"rev":`+jsonQuote(v2)+`,"name":"theirs"}`, http.StatusNotFound)
	postJSON(bob, "/api/shape/notes/history/restore", `{"rev":`+jsonQuote(v2)+`}`, http.StatusNotFound)
	if got := activeOf("device:bob"); got != vb {
		t.Fatalf("bob active moved to %s", got)
	}
	if len(historyOf(bob).Versions) != 1 {
		t.Fatal("bob's history changed")
	}

	// Refusals leave active unchanged: a schema-incompatible target and a
	// target whose evidence fails the current gates.
	activate(narrow, "device:alice")
	activate(v4, "device:alice")
	activate(vbroken, "device:alice")
	activate(v4, "device:alice")
	tipBefore := tipOf(alice)
	if body := postJSON(alice, "/api/shape/notes/history/restore", `{"rev":`+jsonQuote(narrow)+`}`, http.StatusConflict); !strings.Contains(body, "schema") {
		t.Fatalf("incompatible restore = %q, want a schema refusal", body)
	}
	if got := activeOf("device:alice"); got != v4 {
		t.Fatalf("schema-refused restore moved active to %s", got)
	}
	if tipOf(alice) != tipBefore {
		t.Fatal("schema-refused restore appended to the version line")
	}
	if body := postJSON(alice, "/api/shape/notes/history/restore", `{"rev":`+jsonQuote(vbroken)+`}`, http.StatusConflict); strings.TrimSpace(body) == "" {
		t.Fatal("gate-refused restore names no reason")
	}
	if got := activeOf("device:alice"); got != v4 {
		t.Fatalf("gate-refused restore moved active to %s", got)
	}
	if tipOf(alice) != tipBefore {
		t.Fatal("gate-refused restore appended to the version line")
	}
	if got := activeOf("device:bob"); got != vb {
		t.Fatalf("refusals moved bob active to %s", got)
	}
	dataUntouched("after refusals")

	// A rejected preview leaves no workspace: preview a candidate, then
	// reject it, and both the candidate directory and its preview data copy
	// are gone.
	app := &shapedApp{name: "notes", repo: source, opts: opts}
	rejected := productionCandidate(t, app, "6666666666666666", v4, func(work string) {
		p := filepath.Join(work, "public", "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, "<!-- rejected -->\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	get(alice, "/shape/notes/preview/"+rejected.ID+"/", 200)
	post(alice, "/api/shape/notes/candidates/"+rejected.ID+"/reject", 200)
	rejectedDir, err := app.candidateDir("device:alice", rejected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rejectedDir); !os.IsNotExist(err) {
		t.Fatalf("rejected workspace left behind (stat err %v)", err)
	}
	aliceStore, err := shape.OpenAt(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(aliceStore.Dir(), "preview", rejected.ID)); !os.IsNotExist(err) {
		t.Fatalf("rejected preview data left behind (stat err %v)", err)
	}
	get(alice, "/api/shape/notes/candidates/"+rejected.ID, http.StatusNotFound)
	dataUntouched("at the end")

	// A6 remainder: every control on the shape page carries a name, inputs
	// have labels, and status travels over live regions.
	page := get(alice, "/shape/notes/", 200)
	for _, want := range []string{
		`<label for="shape-request"`,
		`aria-label="Send shaping request"`,
		`role="status"`,
		`aria-live="polite"`,
		`aria-label="Version history"`,
		`aria-label="Undo the last restore"`,
		`aria-label="Compare the two selected versions"`,
		`id="history-status"`,
		`id="history-list"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("shape page lacks %q", want)
		}
	}
	// shape.js lives at the console root, outside a notes-scoped device grant,
	// so the operator session fetches it.
	operatorToken, err := sessions.Mint(currentOSUser())
	if err != nil {
		t.Fatal(err)
	}
	operator := withCookie(&http.Cookie{Name: sessionCookie, Value: operatorToken})
	js := get(operator, "/shape.js", 200)
	for _, want := range []string{
		`setAttribute("aria-label"`,
		`"history-name-"`,
		`"history-diff-"`,
		`"Restore version "`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("shape.js lacks %q", want)
		}
	}
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
