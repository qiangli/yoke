package sdlc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IssueChangeEvent is a human steering comment received for an already-tracked
// GitHub issue. It is deliberately independent from a conductor's transport so
// callers can pass the event to the running conductor however they choose.
type IssueChangeEvent struct {
	IssueNumber int       `json:"issue_number"`
	CommentID   int64     `json:"comment_id"`
	Author      string    `json:"author"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
}

type githubIssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// IssueChangeWatermark records the last comment observed for one issue. The
// timestamp lets GitHub narrow later REST requests; the ID makes the boundary
// exact when comments share a timestamp.
type IssueChangeWatermark struct {
	CommentID int64     `json:"comment_id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type issueChangeState struct {
	SchemaVersion string                                     `json:"schema_version"`
	Repositories  map[string]map[string]IssueChangeWatermark `json:"repositories"`
}

// IssueChangesOptions configures a single poll of tracked GitHub issues.
type IssueChangesOptions struct {
	Repo          string
	IssueNumbers  []int
	RunsDir       string
	StateFile     string
	IgnoreAuthors []string // logins whose comments are consumed but not emitted
	GitHubToken   string
}

func issueChangesStatePath(opt IssueChangesOptions) string {
	if strings.TrimSpace(opt.StateFile) != "" {
		return opt.StateFile
	}
	return filepath.Join(runsDirForOption(opt.RunsDir), "issue-change-watermarks.json")
}

func loadIssueChangeState(path string) (issueChangeState, error) {
	state := issueChangeState{SchemaVersion: schemaVersion, Repositories: map[string]map[string]IssueChangeWatermark{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("sdlc: read issue change watermarks: %w", err)
	}
	if state.Repositories == nil {
		state.Repositories = map[string]map[string]IssueChangeWatermark{}
	}
	return state, nil
}

func saveIssueChangeState(path string, state issueChangeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	state.SchemaVersion = schemaVersion
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func normalizedAuthorSet(authors []string) map[string]bool {
	set := make(map[string]bool)
	for _, author := range authors {
		for _, login := range strings.Split(author, ",") {
			if login = strings.ToLower(strings.TrimSpace(login)); login != "" {
				set[login] = true
			}
		}
	}
	return set
}

// PollGitHubIssueChanges fetches comments newer than each tracked issue's saved
// watermark, persists the advanced watermarks, and returns only human-facing
// events. Ignored authors still advance the watermark to avoid feedback loops.
func PollGitHubIssueChanges(ctx context.Context, opt IssueChangesOptions) ([]IssueChangeEvent, error) {
	repo := strings.TrimSpace(opt.Repo)
	if repo == "" {
		return nil, errors.New("sdlc: issue changes requires a repository (owner/name)")
	}
	if len(opt.IssueNumbers) == 0 {
		return nil, errors.New("sdlc: issue changes requires at least one issue number")
	}
	statePath := issueChangesStatePath(opt)
	state, err := loadIssueChangeState(statePath)
	if err != nil {
		return nil, err
	}
	if state.Repositories[repo] == nil {
		state.Repositories[repo] = map[string]IssueChangeWatermark{}
	}
	token := opt.GitHubToken
	if token == "" {
		token = GitHubToken()
	}
	ignored := normalizedAuthorSet(opt.IgnoreAuthors)
	var events []IssueChangeEvent
	for _, number := range opt.IssueNumbers {
		if number <= 0 {
			return nil, fmt.Errorf("sdlc: invalid issue number %d", number)
		}
		key := strconv.Itoa(number)
		watermark := state.Repositories[repo][key]
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number)
		if !watermark.CreatedAt.IsZero() {
			path += "&since=" + url.QueryEscape(watermark.CreatedAt.UTC().Format(time.RFC3339))
		}
		var comments []githubIssueComment
		if err := githubJSON(ctx, "GET", path, token, nil, &comments); err != nil {
			return nil, err
		}
		// The API normally returns oldest first, but sorting makes watermark
		// advancement deterministic for test servers and compatible providers.
		sort.SliceStable(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
		for _, comment := range comments {
			if comment.ID <= watermark.CommentID {
				continue
			}
			if comment.CreatedAt.After(watermark.CreatedAt) || comment.ID > watermark.CommentID {
				watermark = IssueChangeWatermark{CommentID: comment.ID, CreatedAt: comment.CreatedAt}
			}
			if ignored[strings.ToLower(strings.TrimSpace(comment.User.Login))] {
				continue
			}
			events = append(events, IssueChangeEvent{IssueNumber: number, CommentID: comment.ID, Author: comment.User.Login, Body: comment.Body, CreatedAt: comment.CreatedAt})
		}
		state.Repositories[repo][key] = watermark
	}
	if err := saveIssueChangeState(statePath, state); err != nil {
		return nil, err
	}
	return events, nil
}
