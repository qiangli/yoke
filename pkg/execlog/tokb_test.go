// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
)

// threeDayFailure is the minimum evidence that clears the promotion bar.
func threeDayFailure(argv []string) []ev {
	return []ev{
		{day: 4, episode: "ep-a", argv: argv, exit: 1, dim: "compute"},
		{day: 2, episode: "ep-b", argv: argv, exit: 1, dim: "compute"},
		{day: 1, episode: "ep-c", argv: argv, exit: 1, dim: "compute"},
	}
}

func TestPromoteToKBWritesACandidate(t *testing.T) {
	root := seed(t, threeDayFailure([]string{"go", "test", "./hub/..."}))
	store := kb.Open(t.TempDir())

	res, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("want 1 page written, got %+v", res)
	}

	p, err := store.Load(res.Written[0])
	if err != nil {
		t.Fatal(err)
	}

	// The single most important assertion in this file: a recorder may propose,
	// never author. A pipe that could mint validated knowledge is the
	// confabulation vector with a store attached.
	if p.Status != kb.StatusCandidate {
		t.Errorf("a promoted page must be a candidate, got %q", p.Status)
	}
	if p.Type != "gotcha" {
		t.Errorf("type routes the page to its readers, got %q", p.Type)
	}
	if p.Title != "go test ./hub/..." {
		t.Errorf("title should be the template, got %q", p.Title)
	}
	if p.Evidence == "" {
		t.Error("a page with no evidence pointer cannot be checked")
	}
	if !strings.HasPrefix(p.Evidence, "exec://") {
		t.Errorf("evidence must be a resolvable address, got %q", p.Evidence)
	}
	if p.Scope == nil || p.Scope.OS == "" {
		t.Error("a claim must carry the scope it holds in")
	}
	// The counts must be visible, or a reader has to trust the threshold
	// instead of judging the evidence.
	for _, want := range []string{"3 failures", "3 sessions", "3 days"} {
		if !strings.Contains(p.Description+p.Body, want) {
			t.Errorf("evidence %q missing from the page", want)
		}
	}
}

// TestPromoteToKBIsIdempotent — a recorder runs far more often than a human.
// Blind appending is the documented death spiral of agent memory.
func TestPromoteToKBIsIdempotent(t *testing.T) {
	root := seed(t, threeDayFailure([]string{"go", "test", "./hub/..."}))
	store := kb.Open(t.TempDir())

	first, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	second, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Written) != 0 {
		t.Errorf("second pass must not write a new page, got %+v", second.Written)
	}
	if len(second.Updated) != 1 {
		t.Errorf("second pass should update the same page, got %+v", second)
	}
	if first.Written[0] != second.Updated[0] {
		t.Errorf("slug drifted: %q then %q", first.Written[0], second.Updated[0])
	}

	pages, _ := store.List()
	if len(pages) != 1 {
		t.Errorf("want exactly 1 page after two passes, got %d", len(pages))
	}
}

// TestPromoteRespectsHumanOwnership — the moment somebody validates a page it
// stops being ours. Overwriting a judgement with a recount is worse than never
// running.
func TestPromoteRespectsHumanOwnership(t *testing.T) {
	root := seed(t, threeDayFailure([]string{"go", "test", "./hub/..."}))
	store := kb.Open(t.TempDir())

	res, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	slug := res.Written[0]

	// A human validates it and rewrites the body.
	p, _ := store.Load(slug)
	p.Status = kb.StatusValidated
	p.Body = "a human wrote this after finding the actual cause"
	if err := store.Write(p, "promote"); err != nil {
		t.Fatal(err)
	}

	again, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Skipped) != 1 || again.Skipped[0] != slug {
		t.Errorf("a validated page must be skipped, got %+v", again)
	}

	after, _ := store.Load(slug)
	if after.Status != kb.StatusValidated {
		t.Error("the pipe overwrote a human's validation")
	}
	if !strings.Contains(after.Body, "a human wrote this") {
		t.Error("the pipe destroyed a human's body text")
	}
}

// TestPromoteNothingBelowTheBar — the bar is the whole defence against a bad
// afternoon becoming permanent knowledge.
func TestPromoteToKBNothingBelowTheBar(t *testing.T) {
	root := seed(t, []ev{
		{day: 1, episode: "ep-a", argv: []string{"go", "test", "./..."}, exit: 1},
	})
	store := kb.Open(t.TempDir())

	res, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 0 {
		t.Errorf("one failure must not become a page: %+v", res)
	}
	pages, _ := store.List()
	if len(pages) != 0 {
		t.Errorf("kb must stay empty, got %d pages", len(pages))
	}
}

// TestEvidencePointerResolves — the pointer is the whole "kb indexes into other
// stores" claim. If it does not resolve, the page is an assertion.
func TestEvidencePointerResolves(t *testing.T) {
	argv := []string{"go", "test", "./hub/..."}
	root := seed(t, threeDayFailure(argv))
	store := kb.Open(t.TempDir())

	res, err := PromoteToKB(root, store, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := store.Load(res.Written[0])

	ref, err := ParseEvidence(p.Evidence)
	if err != nil {
		t.Fatalf("the page's own evidence pointer does not parse: %v", err)
	}
	recs, _, err := ref.Resolve(root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("the pointer resolved to nothing")
	}
	for _, r := range recs {
		if r.Template != "go test ./hub/..." {
			t.Errorf("resolved the wrong records: %q", r.Template)
		}
	}
}

// TestEvidenceFadesHonestly is the FADE: the stream is pruned, the claim
// survives, and the drill-down says so rather than reporting an empty result
// that reads as "nothing happened".
func TestEvidenceFadesHonestly(t *testing.T) {
	argv := []string{"go", "test", "./hub/..."}
	root := seed(t, threeDayFailure(argv))
	store := kb.Open(t.TempDir())

	res, _ := PromoteToKB(root, store, PromoteDefaults())
	p, _ := store.Load(res.Written[0])
	ref, _ := ParseEvidence(p.Evidence)

	// Retention catches up with the evidence.
	if _, err := Prune(root, PruneOpts{Before: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	_, _, err := ref.Resolve(root)
	if err == nil {
		t.Fatal("resolving pruned evidence must not silently succeed")
	}
	if !strings.Contains(err.Error(), "pruned") {
		t.Errorf("the error must name the fade, got %v", err)
	}

	// And the claim itself is untouched.
	still, err := store.Load(res.Written[0])
	if err != nil {
		t.Fatal("the page must outlive its evidence")
	}
	if still.Evidence != p.Evidence {
		t.Error("the page's pointer must stay, even dangling — it records what WAS cited")
	}
}

func TestEvidenceRefRoundTrip(t *testing.T) {
	cases := []EvidenceRef{
		{From: day("2026-08-03"), To: day("2026-08-05"), N: 12},
		{From: day("2026-08-03"), To: day("2026-08-03"), Episode: "ep-4f2a", Seqs: []uint64{41, 58}},
		{From: day("2026-08-01"), To: day("2026-08-09"), Episode: "ep-x", N: 300},
	}
	for _, in := range cases {
		s := in.String()
		out, err := ParseEvidence(s)
		if err != nil {
			t.Fatalf("ParseEvidence(%q): %v", s, err)
		}
		if out.String() != s {
			t.Errorf("round trip drifted: %q -> %q", s, out.String())
		}
	}
}

// TestLargeEvidenceIsAWindowNotACopy — listing 300 sequence numbers is not a
// citation, it is the corpus wearing a pointer's clothes.
func TestLargeEvidenceIsAWindowNotACopy(t *testing.T) {
	var recs []Record
	base := time.Now().UTC()
	for i := 0; i < 300; i++ {
		recs = append(recs, Record{At: base, Seq: uint64(i), Episode: "ep-a"})
	}
	ref := EvidenceFor(recs)
	if len(ref.Seqs) != 0 {
		t.Errorf("want no per-record seqs past the cap, got %d", len(ref.Seqs))
	}
	if ref.N != 300 {
		t.Errorf("want the count kept, got %d", ref.N)
	}
	if strings.Contains(ref.String(), "seq=") {
		t.Errorf("large refs must be a window + count, got %q", ref.String())
	}
}

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}
