package sdlc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

// GitHub issue intake — the real `--intake-provider github` fetch (previously
// cosmetic). Mirrors the loom intake shape and reuses the shared label logic in
// labels.go, so GitHub and loom select issues the same way. HTTP pattern mirrors
// GitHubDeployStatus (the already-shipped read path).

// githubAPIBase is the API root; overridable in tests.
var githubAPIBase = "https://api.github.com"

// GitHubLabel / GitHubIssue mirror the subset of the GitHub issues API we use.
type GitHubLabel struct {
	Name string `json:"name"`
}

type GitHubIssue struct {
	Number int           `json:"number"`
	Title  string        `json:"title"`
	Body   string        `json:"body"`
	HTML   string        `json:"html_url"`
	State  string        `json:"state"`
	Labels []GitHubLabel `json:"labels"`
	// PullRequest is non-nil when this "issue" is actually a PR — the issues
	// endpoint returns both, and PRs must be skipped for intake.
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

func (i GitHubIssue) labelNames() []string {
	names := make([]string, len(i.Labels))
	for k, l := range i.Labels {
		names[k] = l.Name
	}
	return names
}

// GitHubToken resolves the API token from the environment (GITHUB_TOKEN, then
// GIT_TOKEN) — same convention as outpost git / GitHubSource.
func GitHubToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GIT_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// nextGitHubIssue selects the highest-priority eligible open issue in repo
// ("owner/name"). requireInitiate gates on the sdlc:go label. labelFilter, when
// non-empty, restricts server-side to issues carrying ALL of those labels.
func nextGitHubIssue(ctx context.Context, repo string, labelFilter []string, requireInitiate bool, token string) (*GitHubIssue, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, errors.New("sdlc: github intake requires a repository (owner/name)")
	}
	q := url.Values{}
	q.Set("state", "open")
	q.Set("per_page", "100")
	q.Set("sort", "created")
	q.Set("direction", "asc")
	if len(labelFilter) > 0 {
		q.Set("labels", strings.Join(labelFilter, ","))
	}
	var issues []GitHubIssue
	if err := githubJSON(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+q.Encode(), token, nil, &issues); err != nil {
		return nil, err
	}
	// Order by SDLC priority label (p0 first); stable so created-asc breaks ties.
	sort.SliceStable(issues, func(a, b int) bool {
		return priorityByLabels(issues[a].labelNames()) < priorityByLabels(issues[b].labelNames())
	})
	for _, issue := range issues {
		if issue.PullRequest != nil {
			continue // the issues endpoint also returns PRs — skip
		}
		if issue.State != "" && issue.State != "open" {
			continue
		}
		if eligibleByLabels(issue.labelNames(), issue.Title, requireInitiate) {
			picked := issue
			return &picked, nil
		}
	}
	return nil, nil
}

func githubJSON(ctx context.Context, method, path, token string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(githubAPIBase, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sdlc: GitHub API %s %s: %s\n%s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SelectOptions carries the credentials/policy the provider-dispatching selector
// needs but that don't live on IntakeConfig.
type SelectOptions struct {
	RequireInitiate bool   // gate on sdlc:go (recommended for github)
	GitHubToken     string // defaults to GitHubToken() when empty
	LoomURL         string // loom base URL
	Token           string // loom token
}

// SelectNextIssue picks the next issue from the configured intake provider,
// returning nil when the queue is empty. This is the provider dispatch that makes
// `--intake-provider github` real; loom/gitea keep their existing behavior. For
// `local`/other providers the caller must pass the issue explicitly (--issue).
func SelectNextIssue(ctx context.Context, in IntakeConfig, opts SelectOptions) (*Issue, error) {
	switch strings.ToLower(strings.TrimSpace(in.Provider)) {
	case "github":
		token := opts.GitHubToken
		if token == "" {
			token = GitHubToken()
		}
		gi, err := nextGitHubIssue(ctx, in.Repository, in.Labels, opts.RequireInitiate, token)
		if err != nil || gi == nil {
			return nil, err
		}
		return &Issue{
			ID:    fmt.Sprintf("%s#%d", strings.TrimSpace(in.Repository), gi.Number),
			URL:   gi.HTML,
			Title: gi.Title,
			Body:  gi.Body,
		}, nil
	case "loom", "gitea":
		li, err := nextLoomIssue(ctx, opts.LoomURL, opts.Token, in.Repository)
		if err != nil || li == nil {
			return nil, err
		}
		return &Issue{
			ID:    fmt.Sprintf("%s#%d", strings.TrimSpace(in.Repository), li.Number),
			URL:   li.HTML,
			Title: li.Title,
			Body:  li.Body,
		}, nil
	default:
		return nil, fmt.Errorf("sdlc: intake provider %q does not support automatic issue selection; pass --issue/--issue-file", in.Provider)
	}
}
