package fleet

import (
	"strings"
	"testing"
)

func cloneCatalog(t *testing.T) *Catalog {
	t.Helper()
	return New(WithRoot(t.TempDir()))
}

func seedParent(t *testing.T, cat *Catalog, a Agent) Agent {
	t.Helper()
	if err := cat.SaveAgent(a); err != nil {
		t.Fatalf("seed %s: %v", a.Name, err)
	}
	got, ok := cat.Agent(a.Name)
	if !ok {
		t.Fatalf("seeded agent %s did not load back", a.Name)
	}
	return got
}

// TestCloneInheritsCapabilityNotIdentity is the line the whole model draws.
// A clone is the same CAPABILITY (tool, model, band, role, instruction) under a
// DIFFERENT identity — copying the name, aliases or nick would recreate exactly
// the collision cloning exists to avoid.
func TestCloneInheritsCapabilityNotIdentity(t *testing.T) {
	cat := cloneCatalog(t)
	seedParent(t, cat, Agent{
		Name: "elif", Aliases: []string{"smarty"}, Nick: "Elif",
		Tool: "ycode", Model: "glm-5.2",
		Band: 3, BandSource: "operator",
		Role:        &AgentRole{Skills: []string{"conductor"}, Scope: "repo"},
		Instruction: &AgentInstruction{Content: "be terse"},
		Functions:   []string{"search"},
	})

	clone, err := cat.CloneAgent("elif", "elif2", false, "")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Capability comes across.
	if clone.MatrixKey() != "ycode:glm-5.2" {
		t.Errorf("binding = %q, want the parent's", clone.MatrixKey())
	}
	if clone.Band != 3 || clone.BandSource != "operator" {
		t.Errorf("band = %d/%q, want the parent's contract", clone.Band, clone.BandSource)
	}
	if clone.Role == nil || clone.Role.Scope != "repo" {
		t.Errorf("role = %+v, want the parent's", clone.Role)
	}
	if clone.Instruction == nil || clone.Instruction.Content != "be terse" {
		t.Errorf("instruction = %+v, want the parent's", clone.Instruction)
	}

	// Identity does NOT.
	if clone.Name != "elif2" {
		t.Errorf("name = %q, want the new one", clone.Name)
	}
	if len(clone.Aliases) != 0 {
		t.Errorf("aliases = %v, want none — an alias is the parent's identity", clone.Aliases)
	}
	if clone.Nick != "" {
		t.Errorf("nick = %q, want it redrawn rather than copied", clone.Nick)
	}

	// Provenance is recorded, because a fleet of same-binding agents with no
	// record of what branched from what is unreadable.
	if clone.ClonedFrom != "elif" || clone.ClonedAt == "" {
		t.Errorf("provenance = %q/%q, want parent and timestamp", clone.ClonedFrom, clone.ClonedAt)
	}
}

// TestCloneOfCloneDescribesItsOwnParent — the generated description must track
// cloned_from, or a third-generation clone claims descent from its grandparent.
func TestCloneOfCloneDescribesItsOwnParent(t *testing.T) {
	cat := cloneCatalog(t)
	seedParent(t, cat, Agent{Name: "elif", Tool: "ycode", Model: "glm-5.2"})

	first, err := cat.CloneAgent("elif", "elif2", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(first); err != nil {
		t.Fatal(err)
	}
	second, err := cat.CloneAgent("elif2", "elif3", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ClonedFrom != "elif2" {
		t.Fatalf("cloned_from = %q, want elif2", second.ClonedFrom)
	}
	if !strings.Contains(second.Description, "elif2") {
		t.Errorf("description %q disagrees with cloned_from %q", second.Description, second.ClonedFrom)
	}
}

// TestCloneKeepsAnOperatorWrittenDescription — only the generated one is
// replaced; a description someone wrote is theirs.
func TestCloneKeepsAnOperatorWrittenDescription(t *testing.T) {
	cat := cloneCatalog(t)
	seedParent(t, cat, Agent{
		Name: "elif", Tool: "ycode", Model: "glm-5.2",
		Description: "reviews parser changes",
	})
	clone, err := cat.CloneAgent("elif", "elif2", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if clone.Description != "reviews parser changes" {
		t.Errorf("description = %q, want the parent's own words kept", clone.Description)
	}
}

// TestNextCloneNameSkipsTaken — `agents clone elif` twice must not try to write
// elif2 twice.
func TestNextCloneNameSkipsTaken(t *testing.T) {
	cat := cloneCatalog(t)
	seedParent(t, cat, Agent{Name: "elif", Tool: "ycode", Model: "glm-5.2"})
	seedParent(t, cat, Agent{Name: "elif2", Tool: "ycode", Model: "glm-5.2"})

	got, err := cat.nextCloneName("elif")
	if err != nil {
		t.Fatal(err)
	}
	if got != "elif3" {
		t.Errorf("nextCloneName = %q, want elif3 (elif2 is taken)", got)
	}
}

// TestTaskCloneNameSanitizes — a task id becomes part of a filename, so it is
// sanitized rather than trusted. validName rejects separators; this must never
// hand it one.
func TestTaskCloneNameSanitizes(t *testing.T) {
	for _, tc := range []struct{ task, want string }{
		{"412", "elif-412"},
		{"issue/412", "elif-issue-412"},
		{"../../etc/passwd", "elif-etc-passwd"},
		{"  spaced  id ", "elif-spaced-id"},
		{"", ""},
		{"///", ""},
	} {
		got := taskCloneName("elif", tc.task)
		if got != tc.want {
			t.Errorf("taskCloneName(%q) = %q, want %q", tc.task, got, tc.want)
		}
		if got != "" {
			if err := validName(got); err != nil {
				t.Errorf("taskCloneName(%q) = %q, which validName rejects: %v", tc.task, got, err)
			}
		}
	}
}

// TestCloneContextReportsFreshWhenNoClonerIsWired — the registry must never
// imply a context was branched when the binary has no way to branch one.
func TestCloneContextReportsFreshWhenNoClonerIsWired(t *testing.T) {
	cat := cloneCatalog(t)
	note := cat.cloneContext(Agent{Name: "elif"}, Agent{Name: "elif2"})
	if !strings.Contains(note, "fresh context") {
		t.Errorf("note = %q, want it to say the clone starts fresh", note)
	}
}

// TestCloneContextSurfacesTheClonerError — a refusal (a live parent, say) must
// reach the operator as words, not be swallowed into a cheerful success line.
func TestCloneContextSurfacesTheClonerError(t *testing.T) {
	cat := New(WithRoot(t.TempDir()), WithContextCloner(
		func(parent, clone Agent) (string, error) {
			return "", errLive{}
		}))
	note := cat.cloneContext(Agent{Name: "elif"}, Agent{Name: "elif2"})
	if !strings.Contains(note, "is live") {
		t.Errorf("note = %q, want the cloner's reason", note)
	}
}

type errLive struct{}

func (errLive) Error() string { return "elif is live (pid 1): its store is being written" }
