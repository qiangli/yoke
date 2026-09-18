package sdlc

import (
	"context"
	"strconv"
	"strings"
)

// Intake wiring — the glue that makes `bashy sdlc tick --intake-provider github`
// select the next issue automatically (instead of requiring --issue on the CLI),
// claim it with a lifecycle label so a concurrent tick can't double-pick, and —
// under policy — promote it with the deploy baton. See labels.go + github_*.go.

// policyBool reads a boolean policy value, defaulting when unset/blank.
func policyBool(pol map[string]string, key string, def bool) bool {
	v, ok := pol[key]
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}

// parseIssueRef splits an Issue.ID of the form "owner/name#123" into its repo and
// number. ok is false when the id is not in that shape (e.g. a local issue).
func parseIssueRef(id string) (repo string, number int, ok bool) {
	i := strings.LastIndex(id, "#")
	if i <= 0 || i == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(id[i+1:]))
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(id[:i]), n, true
}

// resolveIntakeIssue fills opt.Issue from the configured provider when no explicit
// issue was supplied. It returns (true, nil) when an issue is now set (either the
// one passed in or a freshly selected one), and (false, nil) when the queue is
// empty (a normal no-op for a scheduler). Only the github provider is
// auto-selected here; loom/local are handled by their own paths.
//
// For github, requireInitiate (policy intake_require_initiate, default TRUE)
// restricts selection to issues carrying the sdlc:go label — so an unattended
// tick only ever acts on a human-blessed issue, never on an arbitrary open one.
func resolveIntakeIssue(ctx context.Context, cfg Config, opt *DelegateOptions) (bool, error) {
	if strings.TrimSpace(opt.Issue.Title) != "" {
		return true, nil // explicit issue already provided
	}
	if strings.ToLower(strings.TrimSpace(cfg.Intake.Provider)) != "github" {
		return false, nil
	}
	requireInitiate := policyBool(cfg.Policies, "intake_require_initiate", true)
	labelFilter := cfg.Intake.Labels
	if requireInitiate {
		// Narrow server-side to blessed issues; the type/priority labels stay
		// advisory (they inform ordering, not a hard AND filter).
		labelFilter = []string{LabelInitiate}
	}
	issue, err := SelectNextIssue(ctx, IntakeConfig{
		Provider:   "github",
		Repository: cfg.Intake.Repository,
		Labels:     labelFilter,
	}, SelectOptions{RequireInitiate: requireInitiate, GitHubToken: GitHubToken()})
	if err != nil {
		return false, err
	}
	if issue == nil {
		return false, nil // empty queue
	}
	opt.Issue = *issue
	return true, nil
}

// claimGitHubIssue applies the sdlc:in-progress label to a freshly selected github
// issue so a concurrent tick skips it (eligibleByLabels treats it as a reserved
// skip label). Best-effort + no-op without a token or a parseable issue ref. Only
// call this for a github auto-selection (guard on DelegateResult.AutoSelected) so
// it never fires against a loom-shaped ref.
func claimGitHubIssue(ctx context.Context, issue Issue) error {
	repo, number, ok := parseIssueRef(issue.ID)
	if !ok {
		return nil
	}
	return applyGitHubLabels(ctx, repo, number, []string{LabelInProgress}, GitHubToken())
}
