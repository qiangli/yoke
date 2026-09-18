// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/issue"
)

// runListJSON drives the real `todo list --json` command against a storeFunc and
// returns stdout. It exercises the cobra command end to end — not a
// re-implementation of the rendering — so the envelope contract is tested at the
// seam a caller actually crosses.
func runListJSON(t *testing.T, sf storeFunc, args ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	cmd := newListCmd(sf)
	cmd.SetOut(&buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("todo list: %v", err)
	}
	return buf.Bytes()
}

// runShow drives `todo show` through its Cobra command so display tests cover
// both the human-facing text and the structured JSON output.
func runShow(t *testing.T, sf storeFunc, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	cmd := newShowCmd(sf)
	cmd.SetOut(&buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("todo show %v: %v", args, err)
	}
	return buf.String()
}

func TestShowRendersSprintAssociationTruthfully(t *testing.T) {
	st := &issue.Store{Root: t.TempDir()}
	associated, err := Add(st, "associated story", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	associated.Sprint = 127
	if _, err := st.Save(associated); err != nil {
		t.Fatal(err)
	}
	unassociated, err := Add(st, "independent task", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	sf := func() (*issue.Store, string, error) { return st, "test", nil }

	if got := runShow(t, sf, associated.ID); !strings.Contains(got, "  sprint    #127\n") {
		t.Errorf("associated text output omitted sprint:\n%s", got)
	}
	if got := runShow(t, sf, unassociated.ID); strings.Contains(got, "  sprint") {
		t.Errorf("unassociated text output invented a sprint:\n%s", got)
	}

	for _, tc := range []struct {
		name       string
		id         string
		wantSprint int64
		present    bool
	}{
		{name: "associated", id: associated.ID, wantSprint: 127, present: true},
		{name: "unassociated", id: unassociated.ID, present: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]json.RawMessage
			out := runShow(t, sf, tc.id, "--json")
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("show JSON did not parse: %v\n%s", err, out)
			}
			raw, present := got["sprint"]
			if present != tc.present {
				t.Fatalf("sprint field present = %t, want %t:\n%s", present, tc.present, out)
			}
			if tc.present {
				var sprint int64
				if err := json.Unmarshal(raw, &sprint); err != nil {
					t.Fatalf("decode sprint: %v", err)
				}
				if sprint != tc.wantSprint {
					t.Errorf("sprint = %d, want %d", sprint, tc.wantSprint)
				}
			}
		})
	}
}

// envelopeShape mirrors the on-the-wire shape with the production result/item
// types, so a schema drift in listResult/listItem fails to parse here.
type envelopeShape struct {
	SchemaVersion string     `json:"schema_version"`
	Command       string     `json:"command"`
	Status        string     `json:"status"`
	Result        listResult `json:"result"`
}

func TestListJSONEnvelopeUserScope(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	pinTodoAgents(t, "alice")
	st, err := UserStore("steward")
	if err != nil {
		t.Fatal(err)
	}
	scope := "user steward"

	// Two open items, one rich (every optional field) and one bare, plus a DONE
	// item that the default (open-only) view must exclude.
	due := time.Now().UTC().AddDate(0, 0, 2)
	rich, err := Add(st, "ship the feature", "body text", "p1", &due, "daily", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := Add(st, "write docs", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	done, err := Add(st, "old work", "", "p2", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(st, done.ID, StatusDone); err != nil {
		t.Fatal(err)
	}

	out := runListJSON(t, func() (*issue.Store, string, error) { return st, scope, nil }, "--json")

	// Parses as valid JSON into the envelope shape — this is the "parses" half
	// of the gate. Unknown fields are allowed (omitempty fields may be absent).
	var env envelopeShape
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope did not parse as JSON: %v\n%s", err, out)
	}

	// Top-level contract: versioned, named, ok.
	if env.SchemaVersion != weavecli.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", env.SchemaVersion, weavecli.SchemaVersion)
	}
	if env.Command != "todo list" {
		t.Errorf("command = %q, want %q", env.Command, "todo list")
	}
	if env.Status != "ok" {
		t.Errorf("status = %q, want %q", env.Status, "ok")
	}

	// Scope/folder carry the resolved context the text header would print.
	if env.Result.Scope != scope {
		t.Errorf("result.scope = %q, want %q", env.Result.Scope, scope)
	}
	wantFolder := filepath.Join(st.Root, st.Sub)
	if env.Result.Folder != wantFolder {
		t.Errorf("result.folder = %q, want %q", env.Result.Folder, wantFolder)
	}

	// Default view is open-only: the done item is excluded, so count == 2.
	if env.Result.Count != 2 {
		t.Errorf("count = %d, want 2 (done excluded by default)", env.Result.Count)
	}
	if len(env.Result.Items) != 2 {
		t.Fatalf("items = %d, want 2:\n%s", len(env.Result.Items), out)
	}

	// Items are priority-first (p1 before unset); the rich item leads.
	got := env.Result.Items[0]
	if got.ID != rich.ID {
		t.Errorf("items[0].id = %q, want %q (priority ordering)", got.ID, rich.ID)
	}
	if got.Title != "ship the feature" {
		t.Errorf("items[0].title = %q", got.Title)
	}
	if got.State != StatusAssigned {
		t.Errorf("items[0].state = %q, want %q", got.State, StatusAssigned)
	}
	if got.Priority != "p1" {
		t.Errorf("items[0].priority = %q, want p1", got.Priority)
	}
	if got.Assignee != "alice" {
		t.Errorf("items[0].assignee = %q, want alice", got.Assignee)
	}
	if got.Recurring != "daily" {
		t.Errorf("items[0].recurring = %q, want daily", got.Recurring)
	}
	if got.Due == nil || !got.Due.Equal(due) {
		t.Errorf("items[0].due = %v, want %v", got.Due, due)
	}
	// Scope repeats per row so an item is self-describing out of context.
	if got.Scope != scope {
		t.Errorf("items[0].scope = %q, want %q", got.Scope, scope)
	}
	// created is the machine timestamp; age is the human duration the text view shows.
	if !got.Created.Equal(rich.Created) {
		t.Errorf("items[0].created = %v, want %v", got.Created, rich.Created)
	}
	if got.Age == "" {
		t.Error("items[0].age is empty; want a human-readable duration")
	}
	if got.Seq != rich.Seq {
		t.Errorf("items[0].seq = %d, want %d", got.Seq, rich.Seq)
	}

	// The bare item carries no optional fields (omitempty drops them).
	b := env.Result.Items[1]
	if b.ID != bare.ID {
		t.Errorf("items[1].id = %q, want %q", b.ID, bare.ID)
	}
	if b.Priority != "" || b.Assignee != "" || b.Due != nil {
		t.Errorf("items[1] should have no optional fields, got %+v", b)
	}
	if b.State != StatusTodo {
		t.Errorf("items[1].state = %q, want %q", b.State, StatusTodo)
	}

	// Raw payload must round-trip through a second, strict decoder into the
	// production item type — proves listItem is the actual on-wire shape.
	var strict struct {
		Result struct {
			Items []listItem `json:"items"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &strict); err != nil {
		t.Fatalf("strict re-parse failed: %v", err)
	}
	if len(strict.Result.Items) != 2 {
		t.Fatalf("strict items = %d, want 2", len(strict.Result.Items))
	}
}

func TestListJSONEnvelopeRepoScope(t *testing.T) {
	// A repo-scope store (docs/todo under a project root) — the "in a repo" half
	// of the gate. Uses --base-dir resolution so no git repo is needed on disk.
	base := t.TempDir()
	bst, label, err := ResolveStore("steward", false, false, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(bst, "committed task", "tracked in git", "p0", nil, "", ""); err != nil {
		t.Fatal(err)
	}

	out := runListJSON(t, func() (*issue.Store, string, error) { return bst, label, nil }, "--json")

	var env envelopeShape
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope did not parse: %v\n%s", err, out)
	}
	if env.SchemaVersion != weavecli.SchemaVersion || env.Command != "todo list" || env.Status != "ok" {
		t.Fatalf("envelope top-level = %+v", env)
	}
	if env.Result.Scope != label {
		t.Errorf("scope = %q, want %q", env.Result.Scope, label)
	}
	if env.Result.Folder != filepath.Join(base, RepoSub) {
		t.Errorf("folder = %q, want %s", env.Result.Folder, filepath.Join(base, RepoSub))
	}
	if env.Result.Count != 1 || len(env.Result.Items) != 1 {
		t.Fatalf("want 1 item, got %d (count=%d):\n%s", len(env.Result.Items), env.Result.Count, out)
	}
	if env.Result.Items[0].Title != "committed task" {
		t.Errorf("title = %q", env.Result.Items[0].Title)
	}
	if env.Result.Items[0].State != StatusTodo {
		t.Errorf("state = %q", env.Result.Items[0].State)
	}
}

func TestListJSONEnvelopeEmpty(t *testing.T) {
	// An empty list still emits a valid envelope with an empty (non-nil) items
	// array — never null, so a consumer can len() it unconditionally.
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, _ := UserStore("steward")

	out := runListJSON(t, func() (*issue.Store, string, error) { return st, "user steward", nil }, "--json")

	var env envelopeShape
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if env.Result.Count != 0 {
		t.Errorf("count = %d, want 0", env.Result.Count)
	}
	if env.Result.Items == nil {
		t.Fatalf("items is nil; want a non-nil empty array")
	}
	if len(env.Result.Items) != 0 {
		t.Errorf("items = %v, want empty", env.Result.Items)
	}
}

func TestListJSONOutputIsPlainTextCompatible(t *testing.T) {
	// The text path is unchanged: without --json the header line and table
	// render exactly as before (no JSON, no envelope leaking into text mode).
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, _ := UserStore("steward")
	if _, err := Add(st, "text mode task", "", "p1", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	out := runListJSON(t, func() (*issue.Store, string, error) { return st, "user steward", nil })

	if strings.HasPrefix(strings.TrimSpace(string(out)), "{") {
		t.Errorf("text mode emitted JSON; expected the table header:\n%s", out)
	}
	if !strings.Contains(string(out), "todo [user]") {
		t.Errorf("text mode missing header line:\n%s", out)
	}
	if !strings.Contains(string(out), "text mode task") {
		t.Errorf("text mode missing task title:\n%s", out)
	}
}
