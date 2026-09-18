package kb

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// runNoDir runs the kb CLI without injecting --dir, so scope resolution (e.g.
// --ring agent → <YCODE_DATA_DIR>/kb) decides the store.
func runNoDir(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewKBCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// scratchEnv points every store-locating env var at a temp dir so a test never
// reads or writes the operator's real ~/.bashy, ~/.config/bashy, ~/.agents or
// ~/.claude. HOME/USERPROFILE cover os.UserHomeDir (DefaultDir, HostID,
// DetectSources); the bashy/ycode vars cover the rest.
func scratchEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_KB_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SKILLS_DIR", t.TempDir())
	t.Setenv("YCODE_DATA_DIR", t.TempDir())
	// A deterministic principal identity, so Source.Tool owner-scoping and the
	// journal stamp do not fall through to $USER (the operator).
	t.Setenv("WEAVE_AGENT", "transfer-test-agent")
}

// TestTransferFromMemex pins the move: the fixture memex dir transfers N notes
// into a scratch ring as form: note candidates tagged xfer:memex, the source
// dir is left byte-identical, and a second run moves nothing (idempotent by
// content hash, then near-duplicate title).
func TestTransferFromMemex(t *testing.T) {
	scratchEnv(t)
	src, err := filepath.Abs(filepath.Join("testdata", "memex"))
	if err != nil {
		t.Fatal(err)
	}
	// cwd out of any git repo so DetectSources probes no real repo store.
	t.Chdir(t.TempDir())

	before := snapshotDir(t, src)
	ring := t.TempDir()

	out := mustRun(t, ring, "transfer", "--from", "memex", src)
	if !strings.Contains(out, "transferred 4 memex note(s)") {
		t.Fatalf("first run did not move 4 notes:\n%s", out)
	}
	if !strings.Contains(out, "skipped: 1 expired, 1 already present") {
		t.Fatalf("first-run skip counts wrong:\n%s", out)
	}
	if !strings.Contains(out, "source left untouched") {
		t.Fatalf("missing source-untouched notice:\n%s", out)
	}

	// The source dir must be untouched — this is a MOVE reported for the
	// operator to delete, never a delete kb performs.
	after := snapshotDir(t, src)
	if len(before) != len(after) {
		t.Fatalf("source file count changed: %d -> %d", len(before), len(after))
	}
	for path, content := range before {
		if after[path] != content {
			t.Errorf("source file %s changed", path)
		}
	}

	// Every moved page is a note-form candidate stamped from memex.
	store := Open(ring)
	pages, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 4 {
		t.Fatalf("ring holds %d pages, want 4", len(pages))
	}
	byslug := map[string]*Page{}
	for _, p := range pages {
		byslug[p.Slug] = p
		if p.Form != FormNote {
			t.Errorf("%s: form = %q, want note", p.Slug, p.Form)
		}
		if p.Status != StatusCandidate {
			t.Errorf("%s: status = %q, want candidate", p.Slug, p.Status)
		}
		if p.Source == nil || p.Source.Tool != memexSource {
			t.Errorf("%s: Source.Tool = %+v, want memex", p.Slug, p.Source)
		}
		if !hasXferTag(p.Tags, memexSource) {
			t.Errorf("%s: missing xfer:memex tag: %v", p.Slug, p.Tags)
		}
	}

	// The expired and duplicate-content memories were not moved.
	for _, absent := range []string{"temporary-workaround-for-broken-ci", "outpost-upgrade-needs-force-flag"} {
		if _, ok := byslug[absent]; ok {
			t.Errorf("unexpected page %q (should have been skipped)", absent)
		}
	}

	// The supersedes chain is preserved in both directions.
	legacy := byslug["legacy-pairing-workflow"]
	modern := byslug["modern-pairing-procedure"]
	if legacy == nil || modern == nil {
		t.Fatalf("supersedes pair missing: legacy=%v modern=%v", legacy, modern)
	}
	if legacy.SupersededBy != "modern-pairing-procedure" {
		t.Errorf("legacy.SupersededBy = %q, want modern-pairing-procedure", legacy.SupersededBy)
	}
	if modern.Supersedes != "legacy-pairing-workflow" {
		t.Errorf("modern.Supersedes = %q, want legacy-pairing-workflow", modern.Supersedes)
	}

	// Original tags are preserved alongside xfer:memex.
	if op := byslug["outpost-deploy-force-on-same-commit"]; op != nil {
		if op.Body == "" || !strings.Contains(strings.ToLower(op.Body), "force") {
			t.Errorf("outpost note body not carried: %q", op.Body)
		}
	}

	// kb sources reports the transfer afterwards (counts only, as today).
	sout := mustRun(t, ring, "sources")
	if !strings.Contains(sout, "memex=4") {
		t.Errorf("sources did not report memex=4:\n%s", sout)
	}

	// Second run is idempotent: nothing new moves.
	out = mustRun(t, ring, "transfer", "--from", "memex", src)
	if !strings.Contains(out, "transferred 0 memex note(s)") {
		t.Fatalf("second run was not idempotent:\n%s", out)
	}
	if pages2, _ := store.List(); len(pages2) != 4 {
		t.Fatalf("second run changed page count to %d, want 4", len(pages2))
	}
}

// TestTransferFromMemexAgentRing exercises the resolution path the feature is
// meant for: --ring agent with YCODE_DATA_DIR set lands the notes under
// <YCODE_DATA_DIR>/kb, not the host or repo store.
func TestTransferFromMemexAgentRing(t *testing.T) {
	scratchEnv(t)
	src, err := filepath.Abs(filepath.Join("testdata", "memex"))
	if err != nil {
		t.Fatal(err)
	}
	agentData := t.TempDir()
	t.Setenv("YCODE_DATA_DIR", agentData)
	t.Chdir(t.TempDir())

	// No --dir: let --ring agent resolve the store to <YCODE_DATA_DIR>/kb.
	out, err := runNoDir(t, "--ring", "agent", "transfer", "--from", "memex", src)
	if err != nil {
		t.Fatalf("transfer --ring agent: %v\n%s", err, out)
	}
	ring := filepath.Join(agentData, "kb")
	if !strings.Contains(out, ring) {
		t.Errorf("transfer did not target the agent ring %s:\n%s", ring, out)
	}
	if pages, err := Open(ring).List(); err != nil || len(pages) != 4 {
		t.Fatalf("agent ring holds %d pages (err=%v), want 4", len(pages), err)
	}
}

// TestTransferFromUnknownSource refuses an unsupported source rather than
// silently doing nothing.
func TestTransferFromUnknownSource(t *testing.T) {
	scratchEnv(t)
	t.Chdir(t.TempDir())
	if _, err := run(t, t.TempDir(), "", "transfer", "--from", "claude-memory"); err == nil {
		t.Fatal("transfer --from claude-memory should error (only memex is supported)")
	}
}
