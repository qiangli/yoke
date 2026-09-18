package recommend

import "testing"

func hasRec(recs []Scored, name string) bool {
	for _, r := range recs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func TestKnownEquivalentAgentFiles(t *testing.T) {
	// The flagship case: the agent looked for CLAUDE.md; AGENTS.md exists and is
	// the equivalent. It must be recommended, ahead of unrelated .md files.
	cands := []string{"AGENTS.md", "README.md", "notes.txt", "main.go"}
	recs := Recommend("CLAUDE.md", cands, 3)
	if !hasRec(recs, "AGENTS.md") {
		t.Fatalf("CLAUDE.md should recommend AGENTS.md; got %+v", recs)
	}
	if recs[0].Name != "AGENTS.md" {
		t.Errorf("AGENTS.md should rank first; got %+v", recs)
	}
}

func TestTypoAndCloseVariant(t *testing.T) {
	cands := []string{"config.yaml", "main.go", "server.go"}
	if recs := Recommend("config.yml", cands, 3); !hasRec(recs, "config.yaml") {
		t.Errorf("config.yml should recommend config.yaml; got %+v", recs)
	}
	if recs := Recommend("sever.go", cands, 3); !hasRec(recs, "server.go") {
		t.Errorf("sever.go typo should recommend server.go; got %+v", recs)
	}
}

func TestNoWeakGuesses(t *testing.T) {
	// A totally unrelated name against unrelated candidates → no recommendation
	// (don't hand the agent noise).
	if recs := Recommend("zebra_widget.rs", []string{"main.go", "util.py"}, 3); len(recs) != 0 {
		t.Errorf("unrelated lookup should yield nothing; got %+v", recs)
	}
}

func TestPathBasenameMatching(t *testing.T) {
	// Missing a nested path; the equivalent lives at repo root as a candidate.
	cands := []string{"AGENTS.md", "docs/guide.md"}
	if recs := Recommend("sub/dir/CLAUDE.md", cands, 3); !hasRec(recs, "AGENTS.md") {
		t.Errorf("nested CLAUDE.md should still recommend AGENTS.md; got %+v", recs)
	}
}

func TestNotFoundTargets(t *testing.T) {
	cases := map[string]string{
		"cat: CLAUDE.md: open /tmp/x/CLAUDE.md: no such file or directory": "/tmp/x/CLAUDE.md",
		"ls: cannot access 'sub/CLAUDE.md': No such file or directory":     "sub/CLAUDE.md",
		"grep: config.yml: No such file or directory":                      "config.yml",
	}
	for stderr, want := range cases {
		got := NotFoundTargets(stderr)
		if len(got) == 0 || got[len(got)-1] != want {
			t.Errorf("NotFoundTargets(%q) = %v, want to contain %q", stderr, got, want)
		}
	}
	if len(NotFoundTargets("permission denied")) != 0 {
		t.Error("non-not-found stderr must yield nothing")
	}
}
