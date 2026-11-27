package shape

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemberViewDropsHostPathsAndIdentities(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	host := filepath.Join(t.TempDir(), "host")
	ws := filepath.Join(host, "shape-work", "key", "0123456789abcdef", "work")
	evidence := filepath.Join(host, "src", ".evidence", "rev1.json")
	full := Candidate{
		ID: "0123456789abcdef", App: "site", Principal: "cloud:member@example.com", Request: "change the footer",
		Status: Failed, BaseRev: "base", Rev: "rev1", Agent: "stub", Artifact: "rev1",
		Error:               "build failed in " + ws + "/pages/index.md; see " + filepath.Join(home, "notes.txt"),
		Workspace:           ws,
		TrackingRoot:        filepath.Join(host, "todo"),
		TrackingItem:        "T-42",
		ArtifactPath:        filepath.Join(host, "artifacts", "site"),
		Actor:               "host-agent",
		ActivationPrincipal: "owner@example.com",
		Identity:            "agent-identity",
		Receipt:             "agent-receipt",
		ProgramPreview:      &ProgramPreview{Kind: "service", Path: filepath.Join(host, "preview"), DataDir: filepath.Join(host, "data")},
		Verdict: &Verdict{Evidence: evidence, ArtifactPath: filepath.Join(host, "artifacts", "site"),
			Detail: "check failed: " + filepath.Join(host, "todo", "x.md") + " and " + filepath.Dir(evidence) + "/rev1.json"},
	}
	saved := *full.Verdict
	got := MemberView(full)

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{host, home, "owner@example.com", "host-agent", "agent-identity", "agent-receipt", "T-42"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("member view leaks %q: %s", leak, data)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workspace", "tracking_root", "tracking_item", "artifact_path", "actor",
		"activation_principal", "agent_identity", "agent_receipt", "program_preview"} {
		if _, ok := fields[key]; ok {
			t.Errorf("member view keeps %s", key)
		}
	}
	if got.Verdict.Evidence != "shape-evidence:rev1" || got.Verdict.ArtifactPath != "" {
		t.Errorf("verdict = %+v", got.Verdict)
	}
	if got.Error != "build failed in pages/index.md; see ~/notes.txt" {
		t.Errorf("error = %q", got.Error)
	}
	if got.Verdict.Detail != "check failed: x.md and rev1.json" {
		t.Errorf("detail = %q", got.Verdict.Detail)
	}
	if got.ID != full.ID || got.Rev != "rev1" || got.BaseRev != "base" || got.Status != Failed ||
		got.Principal != full.Principal || got.Request != full.Request || got.Artifact != "rev1" {
		t.Errorf("member view lost the candidate's own fields: %+v", got)
	}
	if *full.Verdict != saved || full.Workspace != ws {
		t.Error("MemberView changed the caller's candidate")
	}
}

func TestMemberViewOfARequestWithoutVerdict(t *testing.T) {
	got := MemberView(Candidate{ID: "0123456789abcdef", Kind: "request", Status: "requested", ActivationPrincipal: "owner@example.com"})
	if got.Verdict != nil || got.ActivationPrincipal != "" || got.Kind != "request" || got.Status != "requested" {
		t.Errorf("got %+v", got)
	}
}
