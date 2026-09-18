package sdlc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GitHub write side — the conductor drives the lifecycle labels + the deploy
// baton on GitHub, mirroring the loom comment/close helpers. Every function is a
// no-op when token is empty, so a dry run or an unconfigured repo never errors.

// applyGitHubLabels adds labels to an issue (additive; GitHub ignores dupes).
func applyGitHubLabels(ctx context.Context, repo string, number int, labels []string, token string) error {
	if token == "" || len(labels) == 0 {
		return nil
	}
	return githubJSON(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/issues/%d/labels", strings.TrimSpace(repo), number),
		token, map[string][]string{"labels": labels}, nil)
}

// removeGitHubLabel drops a single label; a 404 (label not present) is tolerated
// so clearing a lifecycle label is idempotent.
func removeGitHubLabel(ctx context.Context, repo string, number int, label, token string) error {
	if token == "" || label == "" {
		return nil
	}
	err := githubJSON(ctx, http.MethodDelete,
		fmt.Sprintf("/repos/%s/issues/%d/labels/%s", strings.TrimSpace(repo), number, url.PathEscape(label)),
		token, nil, nil)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil
	}
	return err
}

func commentGitHubIssue(ctx context.Context, repo string, number int, body, token string) error {
	if token == "" || body == "" {
		return nil
	}
	return githubJSON(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/issues/%d/comments", strings.TrimSpace(repo), number),
		token, map[string]string{"body": body}, nil)
}

func closeGitHubIssue(ctx context.Context, repo string, number int, token string) error {
	if token == "" {
		return nil
	}
	return githubJSON(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/issues/%d", strings.TrimSpace(repo), number),
		token, map[string]string{"state": "closed"}, nil)
}

// PromoteByLabel applies the deploy:<env> baton label to a GitHub issue — the
// trigger that fires the deploy GitHub Action. This is the seam between the
// private conductor loop and the public deploy plane: the conductor (after a
// green gate, under policy) or a human (to promote) adds the label; GitHub
// Actions runs the actual deploy. Returns the label applied.
func PromoteByLabel(ctx context.Context, repo string, issueNumber int, env, token string) (string, error) {
	if strings.TrimSpace(env) == "" {
		return "", errors.New("sdlc: promote requires an environment (e.g. dev|qa|prod)")
	}
	if strings.TrimSpace(repo) == "" {
		return "", errors.New("sdlc: promote requires a repository (owner/name)")
	}
	label := DeployLabelForEnv(env)
	if err := applyGitHubLabels(ctx, repo, issueNumber, []string{label}, token); err != nil {
		return "", err
	}
	return label, nil
}

// ResolveGitHubIssue posts a closing comment and closes the issue — the GitHub
// analog of the loom comment+close at the end of a resolved run.
func ResolveGitHubIssue(ctx context.Context, repo string, number int, comment, token string) error {
	if err := commentGitHubIssue(ctx, repo, number, comment, token); err != nil {
		return err
	}
	return closeGitHubIssue(ctx, repo, number, token)
}

// SyncGitHubResolution reflects a run's resolution on its GitHub issue: it clears
// the sdlc:in-progress claim, and for a successful terminal status (resolved /
// rolled-out) applies sdlc:done and — when closeIssue — closes the issue. It is
// a no-op for non-github runs or without a token, so it is always safe to call.
// Returns whether it acted.
func SyncGitHubResolution(ctx context.Context, run RunRecord, status string, closeIssue bool, note, token string) (bool, error) {
	repo, number, ok := parseIssueRef(run.IssueID)
	if !ok || strings.TrimSpace(token) == "" {
		return false, nil
	}
	if err := removeGitHubLabel(ctx, repo, number, LabelInProgress, token); err != nil {
		return false, err
	}
	success := status == "resolved" || status == "rolled-out"
	if success {
		if err := applyGitHubLabels(ctx, repo, number, []string{LabelDone}, token); err != nil {
			return false, err
		}
	}
	if strings.TrimSpace(note) != "" {
		_ = commentGitHubIssue(ctx, repo, number, note, token)
	}
	if success && closeIssue {
		if err := closeGitHubIssue(ctx, repo, number, token); err != nil {
			return false, err
		}
	}
	return true, nil
}

// AdvanceLifecycle moves an issue's sdlc:* status by removing the previous stage
// label (best-effort) and applying the next. Used by the conductor to keep the
// issue's label state board in sync (claim → qa → approved → done).
func AdvanceLifecycle(ctx context.Context, repo string, number int, from, to, token string) error {
	if from != "" {
		if err := removeGitHubLabel(ctx, repo, number, from, token); err != nil {
			return err
		}
	}
	if to != "" {
		return applyGitHubLabels(ctx, repo, number, []string{to}, token)
	}
	return nil
}
