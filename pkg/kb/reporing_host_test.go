package kb

// Repo-ring stores (docs/kb/ inside a git repo) are committed, so writes
// through them record no host; the host ring keeps recording it. Tool and
// Episode are untouched on both rings.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPageWithHost(slug string) *Page {
	return &Page{
		Slug:        slug,
		Form:        FormPage,
		Type:        TypeFact,
		Title:       "gate command " + slug,
		Description: "WHEN testing host stamping",
		Status:      StatusCandidate,
		Source:      &Source{Tool: "test", Host: HostID(), Episode: "ep-test", ParentEpisode: "ep-parent"},
		Body:        "run make gate",
	}
}

func lastJournalLine(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return lines[len(lines)-1]
}

func TestRepoRingWriteRecordsNoHost(t *testing.T) {
	// A temp git checkout with docs/kb, as resolve.go names the repo ring.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, RepoSub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	kbdir := filepath.Join(root, RepoSub)

	// The resolved repo ring (what `kb --repo` / auto-detect open).
	store := openRing(kbdir, "repo")
	if err := store.Write(testPageWithHost("repo-gate"), "add"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(store.PagePath("repo-gate"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "host:") {
		t.Fatalf("repo-ring page front matter records host:\n%s", raw)
	}
	for _, want := range []string{"tool: test", "episode: ep-test"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("repo-ring page lost %q:\n%s", want, raw)
		}
	}

	line := lastJournalLine(t, kbdir)
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["host"]; ok {
		t.Fatalf("repo-ring journal line records host: %s", line)
	}
	// The journal stamps the process identity (ToolID/EpisodeID), not the
	// page's Source — only the host must be gone.
	if rec["tool"] != ToolID() || rec["episode"] != EpisodeID() {
		t.Fatalf("repo-ring journal lost tool/episode: %s", line)
	}

	// Forget is a journal record too: no host there either.
	p, err := store.Load("repo-gate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forget(p, "stale", time.Now()); err != nil {
		t.Fatal(err)
	}
	line = lastJournalLine(t, kbdir)
	if strings.Contains(line, `"host"`) {
		t.Fatalf("repo-ring forget line records host: %s", line)
	}
}

func TestHostRingWriteRecordsHost(t *testing.T) {
	dir := t.TempDir()
	store := openRing(dir, "host")
	if err := store.Write(testPageWithHost("host-gate"), "add"); err != nil {
		t.Fatal(err)
	}

	host := HostID()
	if host == "" {
		t.Fatal("HostID returned empty")
	}
	raw, err := os.ReadFile(store.PagePath("host-gate"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "host: "+host) {
		t.Fatalf("host-ring page front matter missing host %q:\n%s", host, raw)
	}

	line := lastJournalLine(t, dir)
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["host"] != host {
		t.Fatalf("host-ring journal line missing host %q: %s", host, line)
	}
}
