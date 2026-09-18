package binmgr

import "testing"

// TestGithubTokenReadsGHToken locks the ci-failure fix: the ci-failure workflow
// (and gh itself) export GH_TOKEN, not GITHUB_TOKEN — binmgr must honor it, or
// its GitHub API calls go unauthenticated and hit the 60/hr rate limit (HTTP 403
// in CI), which breaks resolving gh itself from cli/cli.
func TestGithubTokenReadsGHToken(t *testing.T) {
	// GH_TOKEN alone must be honored.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GIT_TOKEN", "")
	t.Setenv("GH_TOKEN", "ghtok")
	if got := githubToken(); got != "ghtok" {
		t.Fatalf("githubToken()=%q, want ghtok (GH_TOKEN must be honored)", got)
	}

	// Precedence: GH_TOKEN wins over GITHUB_TOKEN, matching gh's own order.
	t.Setenv("GITHUB_TOKEN", "ghubtok")
	if got := githubToken(); got != "ghtok" {
		t.Fatalf("githubToken()=%q, want ghtok (GH_TOKEN takes precedence)", got)
	}

	// Falls back to GITHUB_TOKEN when GH_TOKEN is empty.
	t.Setenv("GH_TOKEN", "")
	if got := githubToken(); got != "ghubtok" {
		t.Fatalf("githubToken()=%q, want ghubtok (fallback)", got)
	}
}
