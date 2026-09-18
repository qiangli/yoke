package sdlc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// GitHub → loom issue mirror. Copies open GitHub issues into loom as new issues so
// public contributors can file on GitHub while orchestration stays local-first on
// loom. It is IDEMPOTENT (a marker in the mirrored loom body prevents duplicates)
// and DELIBERATELY does NOT bless the mirrored issue (no sdlc:go) — the owner
// triggers by labeling the loom issue, so a public GitHub filer can never kick off
// execution. Run it on a timer (`bashy schedule add --every 5m -- bashy sdlc mirror …`).

type MirrorIssuesOptions struct {
	GitHubRepo  string // owner/name — source of truth for public intake
	GitHubToken string
	LoomURL     string // e.g. http://127.0.0.1:31880
	LoomRepo    string // owner/name in loom — destination
	LoomToken   string
	Labels      []string // optional: only mirror github issues carrying ALL of these
	DryRun      bool
}

type MirroredIssue struct {
	GitHubNumber int      `json:"github_number"`
	GitHubURL    string   `json:"github_url"`
	LoomNumber   int      `json:"loom_number,omitempty"`
	Title        string   `json:"title"`
	Action       string   `json:"action"`                 // created | skipped-existing | dry-run
	LabelsAdded  []string `json:"labels_added,omitempty"` // sdlc:* / deploy:* synced from github
}

type MirrorResult struct {
	SchemaVersion    string          `json:"schema_version"`
	GitHubRepo       string          `json:"github_repo"`
	LoomRepo         string          `json:"loom_repo"`
	Scanned          int             `json:"scanned"`
	Created          int             `json:"created"`
	Private          bool            `json:"private"`           // loom repo visibility, mirrored from GitHub
	VisibilitySynced bool            `json:"visibility_synced"` // whether the mirror set it this run
	Issues           []MirroredIssue `json:"issues,omitempty"`
}

var mirrorMarkerRe = regexp.MustCompile(`mirror:github:[^\s)]+`)

// mirrorMarker is the idempotency token embedded in a mirrored loom issue body.
func mirrorMarker(githubRepo string, num int) string {
	return fmt.Sprintf("mirror:github:%s#%d", strings.Trim(strings.TrimSpace(githubRepo), "/"), num)
}

// MirrorGitHubIssuesToLoom mirrors open GitHub issues into loom, idempotently.
func MirrorGitHubIssuesToLoom(ctx context.Context, opt MirrorIssuesOptions) (MirrorResult, error) {
	ghRepo := strings.Trim(strings.TrimSpace(opt.GitHubRepo), "/")
	loomRepo := strings.Trim(strings.TrimSpace(opt.LoomRepo), "/")
	loomURL := strings.TrimRight(strings.TrimSpace(opt.LoomURL), "/")
	res := MirrorResult{SchemaVersion: schemaVersion, GitHubRepo: ghRepo, LoomRepo: loomRepo}
	if ghRepo == "" || loomRepo == "" || loomURL == "" {
		return res, errors.New("sdlc: mirror requires --github-repo, --loom-repo, and --loom-url")
	}
	ghToken := opt.GitHubToken
	if ghToken == "" {
		ghToken = GitHubToken()
	}
	loomToken := strings.TrimSpace(opt.LoomToken)
	if loomToken == "" {
		loomToken = strings.TrimSpace(os.Getenv("BASHY_LOOM_TOKEN"))
	}
	if loomToken == "" {
		loomToken = strings.TrimSpace(os.Getenv("GITEA_TOKEN"))
	}

	// Mirror the source repo's visibility onto loom so Gitea's OWN ACL matches
	// GitHub — public GitHub repo => public loom repo (any authenticated user can
	// see it), private => private (restricted to owner/collaborators). This is the
	// "keep work local, GitHub is just backup" posture: loom is the source of
	// truth, its access model tracks the mirror source. Best-effort — a
	// visibility hiccup must not block issue mirroring.
	if private, verr := syncLoomRepoVisibility(ctx, ghRepo, ghToken, loomURL, loomToken, loomRepo); verr == nil {
		res.Private, res.VisibilitySynced = private, true
	}

	gh, err := listOpenGitHubIssues(ctx, ghRepo, opt.Labels, ghToken)
	if err != nil {
		return res, err
	}
	res.Scanned = len(gh)

	mirrored, err := existingLoomMirrors(ctx, loomURL, loomToken, loomRepo)
	if err != nil {
		return res, err
	}
	labels := &loomLabelCache{url: loomURL, token: loomToken, repo: loomRepo}

	for _, gi := range gh {
		marker := mirrorMarker(ghRepo, gi.Number)
		mi := MirroredIssue{GitHubNumber: gi.Number, GitHubURL: gi.HTML, Title: gi.Title}
		var loomIssue *LoomIssue
		switch existing, ok := mirrored[marker]; {
		case ok:
			e := existing
			loomIssue, mi.Action, mi.LoomNumber = &e, "skipped-existing", e.Number
		case opt.DryRun:
			mi.Action = "dry-run"
		default:
			body := gi.Body + "\n\n---\nMirrored from " + gi.HTML + " (" + marker + ")"
			var created LoomIssue
			if err := loomJSON(ctx, http.MethodPost, loomURL, loomToken,
				"/api/v1/repos/"+loomRepo+"/issues",
				map[string]string{"title": gi.Title, "body": body}, &created); err != nil {
				return res, fmt.Errorf("sdlc: create loom issue for %s: %w", marker, err)
			}
			loomIssue, mi.LoomNumber, mi.Action = &created, created.Number, "created"
			res.Created++
		}

		// Sync the owner-driven control labels (sdlc:* / deploy:*) github → loom so
		// the owner manages the loop from GitHub. Additive: labels the conductor set
		// on loom are never removed; a control label present on github but missing on
		// loom is added (which fires the label-triggered loom workflow).
		if loomIssue != nil && !opt.DryRun {
			have := labelNameSet(loomIssue.Labels)
			var toAdd []string
			for _, name := range gi.labelNames() {
				if IsControlLabel(name) && !have[normLabel(name)] {
					toAdd = append(toAdd, name)
				}
			}
			if len(toAdd) > 0 {
				ids, err := labels.ensureIDs(ctx, toAdd)
				if err != nil {
					return res, err
				}
				if err := loomJSON(ctx, http.MethodPost, loomURL, loomToken,
					fmt.Sprintf("/api/v1/repos/%s/issues/%d/labels", loomRepo, loomIssue.Number),
					map[string][]int64{"labels": ids}, nil); err != nil {
					return res, fmt.Errorf("sdlc: sync labels to loom#%d: %w", loomIssue.Number, err)
				}
				mi.LabelsAdded = toAdd
			}
		}
		res.Issues = append(res.Issues, mi)
	}
	return res, nil
}

// syncLoomRepoVisibility reads the GitHub repo's `private` flag and sets the
// loom repo's visibility to match, so Gitea's native repo ACL mirrors GitHub
// (public => any authenticated user can see it; private => restricted).
func syncLoomRepoVisibility(ctx context.Context, ghRepo, ghToken, loomURL, loomToken, loomRepo string) (bool, error) {
	var meta struct {
		Private bool `json:"private"`
	}
	if err := githubJSON(ctx, http.MethodGet, "/repos/"+ghRepo, ghToken, nil, &meta); err != nil {
		return false, err
	}
	if err := loomJSON(ctx, http.MethodPatch, loomURL, loomToken,
		"/api/v1/repos/"+loomRepo, map[string]bool{"private": meta.Private}, nil); err != nil {
		return meta.Private, err
	}
	return meta.Private, nil
}

func labelNameSet(labels []LoomLabel) map[string]bool {
	m := make(map[string]bool, len(labels))
	for _, l := range labels {
		m[normLabel(l.Name)] = true
	}
	return m
}

// loomLabelCache resolves loom label name → id, creating any missing label once.
type loomLabelCache struct {
	url, token, repo string
	ids              map[string]int64 // keyed by normLabel(name)
}

func (c *loomLabelCache) load(ctx context.Context) error {
	if c.ids != nil {
		return nil
	}
	var existing []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := loomJSON(ctx, http.MethodGet, c.url, c.token, "/api/v1/repos/"+c.repo+"/labels?limit=100", nil, &existing); err != nil {
		return err
	}
	c.ids = map[string]int64{}
	for _, l := range existing {
		c.ids[normLabel(l.Name)] = l.ID
	}
	return nil
}

func (c *loomLabelCache) ensureIDs(ctx context.Context, names []string) ([]int64, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	var out []int64
	for _, name := range names {
		key := normLabel(name)
		id, ok := c.ids[key]
		if !ok {
			var created struct {
				ID int64 `json:"id"`
			}
			if err := loomJSON(ctx, http.MethodPost, c.url, c.token, "/api/v1/repos/"+c.repo+"/labels",
				map[string]string{"name": name, "color": "#ededed"}, &created); err != nil {
				return nil, fmt.Errorf("sdlc: create loom label %q: %w", name, err)
			}
			id = created.ID
			c.ids[key] = id
		}
		out = append(out, id)
	}
	return out, nil
}

func listOpenGitHubIssues(ctx context.Context, repo string, labels []string, token string) ([]GitHubIssue, error) {
	q := url.Values{}
	q.Set("state", "open")
	q.Set("per_page", "100")
	q.Set("sort", "created")
	q.Set("direction", "asc")
	if len(labels) > 0 {
		q.Set("labels", strings.Join(labels, ","))
	}
	var all []GitHubIssue
	if err := githubJSON(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+q.Encode(), token, nil, &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, i := range all {
		if i.PullRequest != nil { // the issues endpoint also returns PRs
			continue
		}
		out = append(out, i)
	}
	return out, nil // NOTE: single page (≤100 open issues); paginate if a repo exceeds it
}

// newMirrorCmd is `bashy sdlc mirror` — the first EXTERNAL INTAKE adapter into the
// loom/sdlc issue system (GitHub → loom). Safe as a standard/default capability: it
// is INERT (reads GitHub issues, creates loom issues) — no code execution, no
// secrets, and it does NOT bless the mirrored issue, so it can never trigger the
// conductor. Run on a timer: `bashy schedule add --every 5m -- bashy sdlc mirror …`.
func newMirrorCmd() *cobra.Command {
	var opt MirrorIssuesOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "mirror open GitHub issues into loom (first external intake; idempotent; owner still blesses with sdlc:go)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := MirrorGitHubIssuesToLoom(cmd.Context(), opt)
			if err != nil {
				return err
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "mirror %s -> %s: scanned=%d created=%d\n", res.GitHubRepo, res.LoomRepo, res.Scanned, res.Created)
			for _, m := range res.Issues {
				dst := ""
				if m.LoomNumber > 0 {
					dst = fmt.Sprintf(" -> loom#%d", m.LoomNumber)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  gh#%d %s%s\n", m.GitHubNumber, m.Action, dst)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opt.GitHubRepo, "github-repo", "", "source GitHub repo owner/name")
	cmd.Flags().StringVar(&opt.GitHubToken, "github-token", "", "GitHub token; defaults to GITHUB_TOKEN/GIT_TOKEN")
	cmd.Flags().StringVar(&opt.LoomURL, "loom-url", "http://127.0.0.1:31880", "loom base URL")
	cmd.Flags().StringVar(&opt.LoomRepo, "loom-repo", "", "destination loom repo owner/name")
	cmd.Flags().StringVar(&opt.LoomToken, "loom-token", "", "loom API token; defaults to BASHY_LOOM_TOKEN/GITEA_TOKEN")
	cmd.Flags().StringArrayVar(&opt.Labels, "label", nil, "only mirror github issues carrying this label; repeatable")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "list what would be mirrored without creating")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// existingLoomMirrors scans loom issues (open + closed) and maps each mirror marker
// to its loom issue, so re-runs don't duplicate and label sync can diff.
func existingLoomMirrors(ctx context.Context, baseURL, token, repo string) (map[string]LoomIssue, error) {
	var issues []LoomIssue
	if err := loomJSON(ctx, http.MethodGet, baseURL, token,
		"/api/v1/repos/"+repo+"/issues?state=all&type=issues&limit=100", nil, &issues); err != nil {
		return nil, err
	}
	m := map[string]LoomIssue{}
	for _, is := range issues {
		for _, marker := range mirrorMarkerRe.FindAllString(is.Body, -1) {
			m[marker] = is
		}
	}
	return m, nil
}
