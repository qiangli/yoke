package sdlc

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestParseIssueRef(t *testing.T) {
	cases := []struct {
		id     string
		repo   string
		number int
		ok     bool
	}{
		{"acme/app#42", "acme/app", 42, true},
		{"acme/app#0", "acme/app", 0, true},
		{"no-hash", "", 0, false},
		{"#5", "", 0, false},
		{"acme/app#", "", 0, false},
		{"acme/app#x", "", 0, false},
	}
	for _, c := range cases {
		repo, n, ok := parseIssueRef(c.id)
		if ok != c.ok || repo != c.repo || n != c.number {
			t.Fatalf("parseIssueRef(%q) = (%q,%d,%v), want (%q,%d,%v)", c.id, repo, n, ok, c.repo, c.number, c.ok)
		}
	}
}

func TestPolicyBool(t *testing.T) {
	pol := map[string]string{"a": "true", "b": "0", "c": "weird"}
	if !policyBool(pol, "a", false) {
		t.Fatal("a should be true")
	}
	if policyBool(pol, "b", true) {
		t.Fatal("b should be false")
	}
	if !policyBool(pol, "c", true) {
		t.Fatal("c (unparseable) should fall back to default true")
	}
	if !policyBool(pol, "missing", true) {
		t.Fatal("missing should use default")
	}
}

// TestPrepareAutoSelectsGitHubIssue drives the real Prepare path: no --issue,
// provider=github, dry-run (no agent invoked) — it must fetch and select.
func TestPrepareAutoSelectsGitHubIssue(t *testing.T) {
	issues := []GitHubIssue{
		{Number: 9, Title: "add /status", Body: "please", State: "open",
			HTML: "https://github.com/acme/app/issues/9", Labels: lbl("sdlc:go")},
	}
	fakeGitHub(t, issues)

	opt := DelegateOptions{
		DryRun: true,
		Config: ConfigOverrides{
			NoConfig:       true,
			IntakeProvider: "github",
			IntakeRepo:     "acme/app",
			ConductorAgent: "codex",
		},
	}
	res, err := Prepare(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ready" {
		t.Fatalf("status = %q, want ready", res.Status)
	}
	if res.Issue.ID != "acme/app#9" || res.Issue.Title != "add /status" {
		t.Fatalf("unexpected selected issue: %+v", res.Issue)
	}
	if !res.AutoSelected {
		t.Fatal("AutoSelected should be true for a provider selection")
	}
}

// Empty queue → idle status, not an error (a scheduler tick with nothing to do).
func TestPrepareIdleOnEmptyGitHubQueue(t *testing.T) {
	fakeGitHub(t, nil)
	opt := DelegateOptions{
		DryRun: true,
		Config: ConfigOverrides{NoConfig: true, IntakeProvider: "github", IntakeRepo: "acme/app", ConductorAgent: "codex"},
	}
	res, err := Prepare(context.Background(), opt)
	if err != nil {
		t.Fatalf("empty queue should not error: %v", err)
	}
	if res.Status != "idle" {
		t.Fatalf("status = %q, want idle", res.Status)
	}
}

// A non-github provider with no explicit issue keeps the original hard error.
func TestPrepareRequiresIssueForNonGitHub(t *testing.T) {
	opt := DelegateOptions{
		DryRun: true,
		Config: ConfigOverrides{NoConfig: true, IntakeProvider: "local", ConductorAgent: "codex"},
	}
	_, err := Prepare(context.Background(), opt)
	if err == nil {
		t.Fatal("expected --issue-title required error for local provider")
	}
}

// An explicit issue is used as-is and is NOT marked auto-selected.
func TestPrepareUsesExplicitIssue(t *testing.T) {
	opt := DelegateOptions{
		DryRun: true,
		Issue:  Issue{Title: "manual"},
		Config: ConfigOverrides{NoConfig: true, IntakeProvider: "github", IntakeRepo: "acme/app", ConductorAgent: "codex"},
	}
	res, err := Prepare(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Issue.Title != "manual" || res.AutoSelected {
		t.Fatalf("explicit issue mishandled: title=%q auto=%v", res.Issue.Title, res.AutoSelected)
	}
}

func TestClaimGitHubIssue(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	t.Setenv("GITHUB_TOKEN", "tok")
	if err := claimGitHubIssue(context.Background(), Issue{ID: "acme/app#9"}); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 1 || (*reqs)[0].path != "/repos/acme/app/issues/9/labels" {
		t.Fatalf("expected label POST, got %+v", *reqs)
	}
	labels, _ := (*reqs)[0].body["labels"].([]any)
	if len(labels) != 1 || labels[0] != LabelInProgress {
		t.Fatalf("claim should apply %q, got %v", LabelInProgress, (*reqs)[0].body["labels"])
	}
}

// TestTickCommandGitHubIntake drives the REAL `bashy sdlc tick` cobra command
// end-to-end (flags → Delegate → Prepare → github intake) in dry-run against a
// fake GitHub, proving the CLI wiring — not just the Prepare function.
func TestTickCommandGitHubIntake(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{
		{Number: 9, Title: "add /status", State: "open",
			HTML: "https://github.com/acme/app/issues/9", Labels: lbl("sdlc:go")},
	})
	cmd := NewSDLCCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"tick",
		"--no-config", "--intake-provider", "github", "--intake-repo", "acme/app",
		"--conductor", "codex", "--dry-run", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"status":"dry-run"`) {
		t.Fatalf("expected dry-run status, got: %s", got)
	}
	if !strings.Contains(got, "add /status") || !strings.Contains(got, `acme/app#9`) {
		t.Fatalf("tick did not auto-select the github issue: %s", got)
	}
	if !strings.Contains(got, `"auto_selected":true`) {
		t.Fatalf("expected auto_selected=true: %s", got)
	}
}

func TestClaimNoOpOnUnparseableRef(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	t.Setenv("GITHUB_TOKEN", "tok")
	if err := claimGitHubIssue(context.Background(), Issue{ID: "local-only"}); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("claim on a non-github ref should make no HTTP call, got %d", len(*reqs))
	}
}
