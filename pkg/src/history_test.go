package src_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

const (
	s525a10Alice = "device:alice"
	s525a10Bob   = "device:bob"
	s525a10App   = "demoapp"
)

var errS525a10NoEvidence = errors.New("no evidence for this candidate")

func s525a10Write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func s525a10Commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath: dir, Message: msg,
		AuthorName: "s525a10", AuthorEmail: "s525a10@example.com",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return rev.Hash
}

func s525a10Shape(min, max int, rev string) string {
	return "schema_version: bashy-shape-feature-v1\nid: demo\norigin: device:alice\nbase_app: demoapp\nrevision: " + rev +
		"\nschema_range: {min: " + itoa(min) + ", max: " + itoa(max) + "}\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// s525a10Contract is the fixture's shaping contract: a read-only effect
// cap and one smoke check every version tree passes, so the trusted
// verifier judges fixture versions the way it judges the notes app. A tree
// containing BROKEN fails the frozen check, so restoring it is a
// verifier-failing restore.
const s525a10Contract = "@effects(\"read\")\n"

const s525a10Smoke = "set -eu\ntest -f app.txt\ntest -f shape.yaml\n! grep -q BROKEN app.txt\n"

// s525a10Fixture builds a work repo with sequential shaped versions and
// clones it to home/src/demoapp with pristine at the base release:
//
//	p0: base release (pristine)
//	v1..v4: app.txt moves forward; shape.yaml declares range 1..5
//	narrow: app.txt moves again; shape.yaml declares range 1..2
//	vb: bob's own version; shape.yaml declares range 1..5
//	broken: app.txt carries BROKEN; shape.yaml declares range 1..5, so a
//	restore of it fails verification rather than the schema gate
func s525a10Fixture(t *testing.T, home string) (dest, v1, v2, v3, v4, narrow, vb, broken string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if _, err := ygit.Init(ygit.InitOptions{Path: work}); err != nil {
		t.Fatalf("init: %v", err)
	}
	s525a10Write(t, work, "app.txt", "app-base\n")
	s525a10Write(t, work, "contract.bsh", s525a10Contract)
	s525a10Write(t, work, "checks/smoke.bsh", s525a10Smoke)
	p0 := s525a10Commit(t, work, "p0 base release")
	_ = p0

	versions := []struct {
		body         string
		min, max     int
		rev, subject string
	}{
		{"app-v1\n", 1, 5, "v1", "version one"},
		{"app-v2\n", 1, 5, "v2", "version two"},
		{"app-v3\n", 1, 5, "v3", "version three"},
		{"app-v4\n", 1, 5, "v4", "version four"},
		{"app-narrow\n", 1, 2, "narrow", "narrow schema version"},
		{"app-bob\n", 1, 5, "vb", "bob version"},
		{"app-broken\nBROKEN\n", 1, 5, "broken", "broken check version"},
	}
	var out []string
	for _, v := range versions {
		s525a10Write(t, work, "app.txt", v.body)
		s525a10Write(t, work, "shape.yaml", s525a10Shape(v.min, v.max, v.rev))
		out = append(out, s525a10Commit(t, work, v.subject))
	}
	v1, v2, v3, v4, narrow, vb, broken = out[0], out[1], out[2], out[3], out[4], out[5], out[6]

	dest = filepath.Join(home, "src", s525a10App)
	if _, err := gogit.PlainClone(dest, false, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", plumbing.NewHash(p0))); err != nil {
		t.Fatalf("pristine: %v", err)
	}
	return dest, v1, v2, v3, v4, narrow, vb, broken
}

func s525a10Tree(t *testing.T, dest, sha string) string {
	t.Helper()
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		t.Fatal(err)
	}
	return c.TreeHash.String()
}

func s525a10FileAt(t *testing.T, dest, commit, file string) string {
	t.Helper()
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := tr.File(file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func s525a10State(t *testing.T, home, principal string) state.State {
	t.Helper()
	s, err := state.OpenAt(home, s525a10App, principal)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestS525A10VersionHistory is the focused S525.A10 proof on a temp src
// store with two principals: four alice activations read back as history,
// restoring v3 moves the active revision through a new forward commit,
// undoing the restore returns to v4, naming and diffing work, bob is never
// affected, schema-incompatible and gate-refused restores leave the active
// revision unchanged, and both private data directories are untouched.
func TestS525A10VersionHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dest, v1, v2, v3, v4, narrow, vb, _ := s525a10Fixture(t, home)
	gate := state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "ev-test"}, nil
	})

	for principal, body := range map[string]string{s525a10Alice: "alice-private\n", s525a10Bob: "bob-private\n"} {
		s, err := state.OpenAt(home, s525a10App, principal)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteData("note.txt", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	dataUntouched := func(stage string) {
		t.Helper()
		for principal, want := range map[string]string{s525a10Alice: "alice-private\n", s525a10Bob: "bob-private\n"} {
			s, err := state.OpenAt(home, s525a10App, principal)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.ReadData("note.txt")
			if err != nil || string(b) != want {
				t.Fatalf("%s: %s data = %q, want %q", stage, principal, b, want)
			}
		}
	}

	// Four versions for alice, one for bob.
	for _, rev := range []string{v1, v2, v3, v4} {
		if _, err := src.Activate(home, s525a10App, rev, s525a10Alice, src.ActivateOptions{Gate: gate}); err != nil {
			t.Fatalf("activate alice %s: %v", rev[:12], err)
		}
	}
	if _, err := src.Activate(home, s525a10App, vb, s525a10Bob, src.ActivateOptions{Gate: gate}); err != nil {
		t.Fatalf("activate bob: %v", err)
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v4 {
		t.Fatalf("alice active = %s, want %s", st.ActiveRevision, v4)
	}
	bobTip := func() string {
		t.Helper()
		h, err := src.History(home, s525a10App, s525a10Bob)
		if err != nil {
			t.Fatal(err)
		}
		return h.Tip
	}
	bobTipBefore := bobTip()

	// History lists the principal line newest-first with the A7 trailers.
	h, err := src.History(home, s525a10App, s525a10Alice)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h.Entries) != 4 {
		t.Fatalf("history entries = %d, want 4", len(h.Entries))
	}
	for i, want := range []string{v4, v3, v2, v1} {
		e := h.Entries[i]
		if e.Revision != want {
			t.Fatalf("entry %d revision = %s, want %s", i, e.Revision, want)
		}
		if e.Request != want || e.Verdict != "pass" || e.Evidence != "ev-test" || e.Principal != s525a10Alice {
			t.Fatalf("entry %d = %+v, want request/verdict/evidence/principal trailers", i, e)
		}
		if e.Author == "" || e.When.IsZero() || e.Activation == "" {
			t.Fatalf("entry %d = %+v, want author, time, and activation commit", i, e)
		}
	}
	if h.Active != v4 {
		t.Fatalf("history active = %s, want %s", h.Active, v4)
	}

	// Restore v3: a new forward commit whose tree equals v3, then activate.
	restoreGate := gate
	before, err := src.History(home, s525a10App, s525a10Alice)
	if err != nil {
		t.Fatal(err)
	}
	res, err := src.Restore(home, s525a10App, v3, s525a10Alice, src.RestoreOptions{Gate: restoreGate})
	if err != nil {
		t.Fatalf("restore v3: %v", err)
	}
	if res.Revision != v3 || res.Previous != v4 {
		t.Fatalf("restore result = %+v, want revision v3 previous v4", res)
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v3 {
		t.Fatalf("after restore alice active = %s, want v3", st.ActiveRevision)
	}
	if got := s525a10Tree(t, dest, res.Activation); got != s525a10Tree(t, dest, v3) {
		t.Fatal("restore commit tree does not equal the target version tree")
	}
	after, err := src.History(home, s525a10App, s525a10Alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Entries) != 5 || after.Tip == before.Tip {
		t.Fatal("restore did not append a forward commit to the principal line")
	}
	if after.Entries[0].Kind != "restore" || after.Entries[0].Revision != v3 {
		t.Fatalf("newest entry = %+v, want a restore of v3", after.Entries[0])
	}

	// Undo the restore by restoring the pre-restore version.
	if _, err := src.Restore(home, s525a10App, v4, s525a10Alice, src.RestoreOptions{Gate: gate}); err != nil {
		t.Fatalf("undo restore: %v", err)
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v4 {
		t.Fatalf("after undo alice active = %s, want v4", st.ActiveRevision)
	}

	// Name one version; diff two.
	if _, err := src.Name(home, s525a10App, s525a10Alice, v2, "good"); err != nil {
		t.Fatalf("name: %v", err)
	}
	h, err = src.History(home, s525a10App, s525a10Alice)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range h.Named {
		if n.Name == "good" && n.Revision == v2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("named versions = %+v, want good -> v2", h.Named)
	}
	patch, err := src.Diff(home, s525a10App, s525a10Alice, v1, v2)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(patch, "app-v1") || !strings.Contains(patch, "app-v2") {
		t.Fatalf("diff v1..v2 = %q, want both version markers", patch)
	}
	basePatch, err := src.Diff(home, s525a10App, s525a10Alice, "base", v1)
	if err != nil || !strings.Contains(basePatch, "app-v1") {
		t.Fatalf("diff base..v1 = %q, %v", basePatch, err)
	}
	if _, err := src.Diff(home, s525a10App, s525a10Alice, v1, "no-such-version"); err == nil {
		t.Fatal("diff with an unknown revision = nil error, want refusal")
	}

	// Preview leaves the active revision alone and shows the old tree.
	previewDir, err := src.Preview(home, s525a10App, s525a10Alice, v2)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if st, err := os.Stat(filepath.Join(previewDir, "app.txt")); err != nil || st.IsDir() {
		t.Fatalf("preview workspace at %s has no app.txt", previewDir)
	}
	if b, err := os.ReadFile(filepath.Join(previewDir, "app.txt")); err != nil || string(b) != "app-v2\n" {
		t.Fatalf("preview app.txt = %q, want the v2 tree", b)
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v4 {
		t.Fatalf("preview moved alice's active revision to %s", st.ActiveRevision)
	}

	// Bob's history and active revision are never affected.
	bh, err := src.History(home, s525a10App, s525a10Bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(bh.Entries) != 1 || bh.Entries[0].Revision != vb {
		t.Fatalf("bob history = %+v, want only the bob activation", bh.Entries)
	}
	if st := s525a10State(t, home, s525a10Bob); st.ActiveRevision != vb {
		t.Fatalf("bob active = %s, want %s", st.ActiveRevision, vb)
	}
	if bh.Tip != bobTipBefore {
		t.Fatal("alice's restores moved bob's version line")
	}

	// A target that cannot read the current data schema refuses restore.
	aliceTip := func() string {
		t.Helper()
		x, err := src.History(home, s525a10App, s525a10Alice)
		if err != nil {
			t.Fatal(err)
		}
		return x.Tip
	}
	tipBefore := aliceTip()
	if _, err := src.Restore(home, s525a10App, narrow, s525a10Alice, src.RestoreOptions{Gate: gate}); err == nil ||
		!strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema-incompatible restore err = %v, want a schema refusal", err)
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v4 {
		t.Fatalf("schema-refused restore moved active to %s", st.ActiveRevision)
	}
	if aliceTip() != tipBefore {
		t.Fatal("schema-refused restore appended to the version line")
	}

	// A gate refusal also refuses restore and leaves everything unchanged.
	refuse := state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{}, errS525a10NoEvidence
	})
	if _, err := src.Restore(home, s525a10App, v1, s525a10Alice, src.RestoreOptions{Gate: refuse}); err == nil {
		t.Fatal("gate-refused restore = nil error, want refusal")
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v4 {
		t.Fatalf("gate-refused restore moved active to %s", st.ActiveRevision)
	}
	if aliceTip() != tipBefore {
		t.Fatal("gate-refused restore appended to the version line")
	}
	dataUntouched("at the end")

	// The CLI verbs ride the same paths: history text and JSON, preview,
	// diff, and name. A third principal naming another line's version is
	// refused as unknown, the same scoping the web route enforces.
	src.RegisterActivationGate(gate)
	defer src.RegisterActivationGate(nil)
	runCLI := func(args ...string) (string, error) {
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	if out, err := runCLI("history", s525a10App, "--user", s525a10Alice); err != nil ||
		!strings.Contains(out, "pass") || !strings.Contains(out, "ev-test") || !strings.Contains(out, "good") {
		t.Fatalf("history CLI = %q, %v", out, err)
	}
	if out, err := runCLI("history", s525a10App, "--user", s525a10Alice, "--json"); err != nil {
		t.Fatalf("history --json CLI: %v", err)
	} else {
		var decoded struct {
			Entries  []map[string]any `json:"entries"`
			Versions []map[string]any `json:"versions"`
		}
		if err := json.Unmarshal([]byte(out), &decoded); err != nil || len(decoded.Versions) == 0 {
			t.Fatalf("history --json = %q, want a JSON history with versions", out)
		}
		if len(decoded.Entries) != len(decoded.Versions) {
			t.Fatalf("history --json entries = %d, versions = %d; want the entries alias to mirror versions",
				len(decoded.Entries), len(decoded.Versions))
		}
	}
	if out, err := runCLI("preview", s525a10App, v1, "--user", s525a10Alice); err != nil ||
		!strings.Contains(out, "preview ready at") || !strings.Contains(out, "active revision unchanged") {
		t.Fatalf("preview CLI = %q, %v", out, err)
	}
	if out, err := runCLI("diff", s525a10App, v1, v2, "--user", s525a10Alice); err != nil ||
		!strings.Contains(out, "app-v1") {
		t.Fatalf("diff CLI = %q, %v", out, err)
	}
	if out, err := runCLI("name", s525a10App, v1, "first", "--user", "device:carol"); err != nil ||
		!strings.Contains(out, "named first") {
		t.Fatalf("name CLI = %q, %v", out, err)
	}
	// Carol names a version from another principal's line: the shared
	// restore resolves only the caller's own line, so the CLI refuses with
	// the unknown-version reason and nothing moves.
	if out, err := runCLI("restore", s525a10App, v1, "--user", "device:carol"); err == nil ||
		!strings.Contains(err.Error(), "unknown version") {
		t.Fatalf("cross-principal restore CLI = %q, %v; want an unknown-version refusal", out, err)
	}
	t.Setenv("BASHY_PRINCIPAL", "")
	if _, err := runCLI("history", s525a10App); err == nil {
		t.Fatal("history CLI without a principal = nil error, want refusal")
	}
	if st := s525a10State(t, home, s525a10Alice); st.ActiveRevision != v4 {
		t.Fatalf("carol's CLI verbs moved alice's active revision to %s", st.ActiveRevision)
	}
	if st := s525a10State(t, home, s525a10Bob); st.ActiveRevision != vb {
		t.Fatalf("carol's CLI verbs moved bob's active revision to %s", st.ActiveRevision)
	}
	dataUntouched("after the CLI")
}
