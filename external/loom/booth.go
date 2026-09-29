package loom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// CreateBooth gives one user one private repository. Gitea cannot fork a
// private repository for a user who cannot read its source, so the caller
// copies the pinned base into this empty private repository with CopyBoothBase.
// The returned URL never contains a password.
func (c ArenaClient) CreateBooth(org, repo, user, password string) (string, error) {
	if org == "" || repo == "" || user == "" || password == "" ||
		strings.ContainsAny(org+repo+user, "/\\@ ") {
		return "", fmt.Errorf("invalid booth identity")
	}
	if err := c.request(http.MethodPost, "/admin/users", map[string]any{
		"username": user, "password": password, "email": user + "@localhost.invalid",
		"must_change_password": false, "visibility": "private",
	}, http.StatusCreated, http.StatusConflict); err != nil {
		return "", err
	}
	if err := c.request(http.MethodPost, "/admin/users/"+url.PathEscape(user)+"/repos", map[string]any{
		"name": repo, "private": true, "auto_init": false,
	}, http.StatusCreated, http.StatusConflict); err != nil {
		return "", err
	}
	if err := c.boothRequirePrivate(user, repo); err != nil {
		return "", err
	}
	return strings.TrimRight(c.URL, "/") + "/" + user + "/" + repo + ".git", nil
}

func (c ArenaClient) boothRequirePrivate(user, repo string) error {
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/api/v1/repos/"+url.PathEscape(user)+"/"+url.PathEscape(repo), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(LoopbackUser, LoopbackPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("booth repository inspection: HTTP %d", resp.StatusCode)
	}
	var state struct {
		Private bool `json:"private"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&state); err != nil {
		return err
	}
	if !state.Private {
		return fmt.Errorf("booth repository is not private")
	}
	return nil
}

// CopyBoothBase pushes only the pinned base branch; no upstream remote is
// configured in the booth workspace.
func (c ArenaClient) CopyBoothBase(org, repo, forkURL, user, password string) error {
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	baseURL := strings.TrimRight(c.URL, "/") + "/" + org + "/" + repo + ".git"
	return boothCopyBase(ctx, baseURL, forkURL,
		&githttp.BasicAuth{Username: LoopbackUser, Password: LoopbackPassword},
		&githttp.BasicAuth{Username: user, Password: password})
}

func boothCopyBase(ctx context.Context, baseURL, forkURL string, sourceAuth, forkAuth transport.AuthMethod) error {
	dir, err := os.MkdirTemp("", "loom-booth-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	r, err := gogit.PlainCloneContext(ctx, dir, true, &gogit.CloneOptions{
		URL: baseURL, NoCheckout: true, ReferenceName: plumbing.NewBranchReferenceName("base"),
		Auth: sourceAuth,
	})
	if err != nil {
		return fmt.Errorf("copy arena base: %w", err)
	}
	ref, err := r.Reference(plumbing.NewRemoteReferenceName("origin", "base"), true)
	if err != nil {
		return err
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("booth-base"), ref.Hash())); err != nil {
		return err
	}
	remote, err := r.CreateRemoteAnonymous(&config.RemoteConfig{Name: "anonymous", URLs: []string{forkURL}})
	if err != nil {
		return err
	}
	return remote.PushContext(ctx, &gogit.PushOptions{RemoteName: "anonymous", RefSpecs: []config.RefSpec{"+refs/heads/booth-base:refs/heads/base"}, Auth: forkAuth})
}
