package webconsole

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

func TestAppTidySupportWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previous := tidyShapeOptions
	tidyShapeOptions = func(home string) ShapeOptions {
		return ShapeOptions{Home: home, Runner: []string{exe, boundaryInterpArg}}
	}
	defer func() { tidyShapeOptions = previous }()
	source, v1, v2, v3, v4, _, _, _ := historyFixture(t, home)
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(source, "api", "tidy.md")
	if err := os.WriteFile(unit, []byte(unitDoc("echo tidied > tidy.txt")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "shaped.json"), []byte(`{"tidy":{"keep_last":1,"keep_days":1},"properties":["new"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	v5hash, err := wt.Commit("notes: tidy-capable", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	v5 := v5hash.String()
	gate := state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "tidy-test"}, nil
	})
	for _, rev := range []string{v1, v2, v3, v4, v5} {
		if _, err := src.Activate(home, "notes", rev, "device:alice", src.ActivateOptions{Gate: gate}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.Activate(home, "notes", v1, "device:bob", src.ActivateOptions{Gate: gate}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Name(home, "notes", "device:alice", v2, "milestone"); err != nil {
		t.Fatal(err)
	}
	// Offerings publish tagged releases only (S526.B8): release v1 first.
	if _, err := src.Release(home, "notes", v1, "device:alice", src.ReleaseOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Offer(home, "notes", v1, "device:alice", src.OfferOptions{To: []string{"device:bob"}, ID: "early"}); err != nil {
		t.Fatal(err)
	}
	st, err := state.OpenAt(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	s.Adopted = []state.Adopted{{ID: "friend", Rev: v3}}
	s.LastKnownGood = ""
	if err := st.Write(s); err != nil {
		t.Fatal(err)
	}
	before, err := src.History(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := src.History(home, "notes", "device:bob")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		c := NewAppsCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatalf("app %v: %v / %s", args, err, out.String())
		}
		return out.String()
	}
	preview := run("tidy", "notes", "--user", "device:alice", "--keep-last", "1", "--keep-days", "0")
	if !strings.Contains(preview, "squash") || !strings.Contains(preview, v4) || strings.Contains(preview, "squash "+before.Entries[4].Activation) {
		t.Fatalf("preview = %s", preview)
	}
	if strings.Contains(preview, v2) && strings.Contains(preview, "squash "+before.Entries[3].Activation) {
		t.Fatalf("named version squashed: %s", preview)
	}
	unchanged, err := src.History(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Tip != before.Tip || len(unchanged.Entries) != len(before.Entries) {
		t.Fatal("preview changed history")
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), "data", "tidy.txt")); !os.IsNotExist(err) {
		t.Fatalf("preview ran unit: %v", err)
	}
	apply := run("tidy", "notes", "--user", "device:alice", "--keep-last", "1", "--keep-days", "0", "--apply")
	if !strings.Contains(apply, "tidy applied") {
		t.Fatal(apply)
	}
	after, err := src.History(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Entries) != 4 || after.Base == before.Base || after.Tip == before.Tip {
		t.Fatalf("history after tidy: %+v", after)
	}
	for _, want := range []string{v5, v3, v2, v1} {
		found := false
		for _, e := range after.Entries {
			if e.Revision == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("kept %s missing", want)
		}
	}
	for _, lost := range []string{v4} {
		for _, e := range after.Entries {
			if e.Revision == lost {
				t.Fatalf("retired %s retained", lost)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(st.Dir(), "data", "tidy.txt")); err != nil || strings.TrimSpace(string(b)) != "tidied" {
		t.Fatalf("unit not invoked: %q %v", b, err)
	}
	for _, e := range after.Entries {
		if _, err := src.Restore(home, "notes", e.Revision, "device:alice", src.RestoreOptions{Gate: gate}); err != nil {
			t.Fatalf("kept version %s not restorable: %v", e.Revision, err)
		}
	}
	bobAfter, err := src.History(home, "notes", "device:bob")
	if err != nil {
		t.Fatal(err)
	}
	if bobAfter.Tip != bob.Tip || len(bobAfter.Entries) != len(bob.Entries) {
		t.Fatal("bob's history changed")
	}
}
