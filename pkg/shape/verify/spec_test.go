package verify

import (
	"slices"
	"strings"
	"testing"
)

func TestSpecChangeListsEditedAndRemovedBaseChecks(t *testing.T) {
	base := Contract{Checks: []Check{{Name: "checks/a.bsh", Body: "a\n"}, {Name: "checks/b.bsh", Body: "b\n"}, {Name: "checks/c.bsh", Body: "c\n"}}}
	cand := Contract{Checks: []Check{{Name: "checks/a.bsh", Body: "a\n"}, {Name: "checks/b.bsh", Body: "B\n"}, {Name: "checks/d.bsh", Body: "added\n"}}}
	got := SpecChange(base, cand)
	if want := []string{"checks/b.bsh (changed)", "checks/c.bsh (removed)"}; !slices.Equal(got, want) {
		t.Fatalf("SpecChange = %v, want %v (an added check is not a spec change)", got, want)
	}
	if got := SpecChange(base, base); len(got) != 0 {
		t.Fatalf("an unchanged spec reports %v", got)
	}
}

// gradingSpec is the authorization table: no review refuses, a review of
// exactly the candidate's spec grades it by its own checks under the base
// cap, a review of another spec refuses, and a recorded version (restore)
// is graded by its own spec and cap.
func TestGradingSpecAuthorizesOnlyAReviewedOrRecordedSpec(t *testing.T) {
	base := Contract{Effects: []string{"read"}, Checks: []Check{{Name: "checks/a.bsh", Body: "S1\n", BodyDigest: "1"}}}
	cand := Contract{Effects: []string{"read", "write"}, Checks: []Check{{Name: "checks/a.bsh", Body: "S2\n", BodyDigest: "2"}}}
	changed := SpecChange(base, cand)

	if g, _, from, refusal := gradingSpec(base, base, "", nil); refusal != "" || from != "" || g.Digest() != base.Digest() {
		t.Fatalf("unchanged spec: from %q refusal %q", from, refusal)
	}
	if _, _, _, refusal := gradingSpec(base, cand, "", changed); !strings.Contains(refusal, ErrSpecUnreviewed.Error()) || !strings.Contains(refusal, "checks/a.bsh (changed)") {
		t.Fatalf("unreviewed spec change refusal = %q", refusal)
	}
	if _, _, _, refusal := gradingSpec(base, cand, base.Digest(), changed); !strings.Contains(refusal, "is not the candidate's spec") {
		t.Fatalf("review of another spec refusal = %q", refusal)
	}
	g, capFrom, from, refusal := gradingSpec(base, cand, cand.Digest(), changed)
	if refusal != "" || from != SpecFromCandidate || g.Digest() != cand.Digest() || capFrom.Cap() != "read" {
		t.Fatalf("reviewed spec: graded %s cap %q from %q refusal %q", g.Digest(), capFrom.Cap(), from, refusal)
	}
	g, capFrom, from, refusal = gradingSpec(base, cand, VersionSpec, changed)
	if refusal != "" || from != SpecFromCandidate || g.Digest() != cand.Digest() || capFrom.Cap() != "read,write" {
		t.Fatalf("recorded version: graded %s cap %q from %q refusal %q", g.Digest(), capFrom.Cap(), from, refusal)
	}
}
