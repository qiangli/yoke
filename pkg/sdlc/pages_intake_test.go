package sdlc

import (
	"context"
	"testing"
)

func TestResolvePagesIntake_Direct(t *testing.T) {
	// --issue text wins; notifier kind = local (no forge).
	opt := PagesOnceOptions{IssueText: "Fix the footer link\n\nSwap old for new."}
	iss, target, err := resolvePagesIntake(context.Background(), &opt)
	if err != nil {
		t.Fatal(err)
	}
	if iss == nil || iss.Title != "Fix the footer link" {
		t.Fatalf("direct issue not parsed: %+v", iss)
	}
	if target.kind != "local" {
		t.Fatalf("kind = %q, want local", target.kind)
	}
}

func TestResolvePagesIntake_GitHub(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{
		{Number: 3, Title: "update homepage", State: "open", HTML: "https://github.com/acme/site/issues/3"},
	})
	opt := PagesOnceOptions{Provider: "github", IntakeRepo: "acme/site", GitHubToken: "tok"}
	iss, target, err := resolvePagesIntake(context.Background(), &opt)
	if err != nil {
		t.Fatal(err)
	}
	if iss == nil || iss.ID != "acme/site#3" || iss.Title != "update homepage" {
		t.Fatalf("github issue mapping: %+v", iss)
	}
	if target.kind != "github" || target.repo != "acme/site" || target.number != 3 {
		t.Fatalf("github notifier target: %+v", target)
	}
}

func TestResolvePagesIntake_GitHubIdle(t *testing.T) {
	fakeGitHub(t, nil)
	opt := PagesOnceOptions{Provider: "github", IntakeRepo: "acme/site", GitHubToken: "tok"}
	iss, _, err := resolvePagesIntake(context.Background(), &opt)
	if err != nil {
		t.Fatal(err)
	}
	if iss != nil {
		t.Fatalf("empty queue should be nil, got %+v", iss)
	}
}

func TestResolvePagesIntake_UnknownProvider(t *testing.T) {
	opt := PagesOnceOptions{Provider: "svn", IntakeRepo: "x"}
	if _, _, err := resolvePagesIntake(context.Background(), &opt); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// RunPagesOnce dry-run with GitHub intake selects the issue without side effects.
func TestRunPagesOnceDryRunGitHub(t *testing.T) {
	fakeGitHub(t, []GitHubIssue{
		{Number: 8, Title: "new blog post", State: "open", HTML: "https://github.com/acme/site/issues/8"},
	})
	res, err := RunPagesOnce(context.Background(), PagesOnceOptions{
		Provider: "github", IntakeRepo: "acme/site", GitHubToken: "tok",
		WorkspaceDir: t.TempDir(), DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "dry-run" || res.Issue == nil || res.Issue.ID != "acme/site#8" {
		t.Fatalf("unexpected dry-run result: %+v", res)
	}
}

// RunPagesOnce dry-run with a direct issue (no forge).
func TestRunPagesOnceDryRunDirect(t *testing.T) {
	res, err := RunPagesOnce(context.Background(), PagesOnceOptions{
		IssueText: "tweak the about page", WorkspaceDir: t.TempDir(), DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "dry-run" || res.Issue == nil || res.Issue.Title != "tweak the about page" {
		t.Fatalf("unexpected direct dry-run result: %+v", res)
	}
}

func TestRunPagesOnceRequiresWorkspace(t *testing.T) {
	if _, err := RunPagesOnce(context.Background(), PagesOnceOptions{IssueText: "x"}); err == nil {
		t.Fatal("expected --workspace required error")
	}
}
