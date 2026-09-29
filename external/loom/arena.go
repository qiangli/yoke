package loom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ArenaClient administers sprint repositories through the local Gitea API.
type ArenaClient struct {
	URL     string
	Context context.Context
}

// ArenaState reports the managed daemon and its configured public URL.
func ArenaState(ctx context.Context) (State, bool, error) {
	st, err := readState(DefaultDataDir())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	return st, healthy(ctx, st.URL, 2*time.Second), nil
}

func (c ArenaClient) request(method, path string, payload any, allowed ...int) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(LoopbackUser, LoopbackPassword)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, code := range allowed {
		if resp.StatusCode == code {
			return nil
		}
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("loom API %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
}

func (c ArenaClient) EnsureOrg(org string) error {
	path := "/orgs/" + url.PathEscape(org)
	if err := c.request(http.MethodGet, path, nil, http.StatusOK); err == nil {
		return nil
	}
	return c.request(http.MethodPost, "/admin/users/"+LoopbackUser+"/orgs", map[string]any{"username": org, "visibility": "private"}, http.StatusCreated, http.StatusConflict)
}

func (c ArenaClient) EnsureRepo(org, repo string) error {
	path := "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo)
	if err := c.request(http.MethodGet, path, nil, http.StatusOK); err == nil {
		return nil
	}
	return c.request(http.MethodPost, "/orgs/"+url.PathEscape(org)+"/repos", map[string]any{"name": repo, "private": true, "auto_init": false}, http.StatusCreated, http.StatusConflict)
}

func (c ArenaClient) arenaRepoEndpoint(org, name string) (string, transport.AuthMethod) {
	endpoint := strings.TrimRight(c.URL, "/") + "/" + org + "/" + name + ".git"
	parsed, err := url.Parse(endpoint)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return endpoint, &githttp.BasicAuth{Username: LoopbackUser, Password: LoopbackPassword}
	}
	return endpoint, nil
}

func (c ArenaClient) PushBase(org, name, root, sha string) error {
	dir, err := os.MkdirTemp("", "arena-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := gogit.PlainCloneContext(ctx, dir, true, &gogit.CloneOptions{URL: root, NoCheckout: true})
	if err != nil {
		return err
	}
	hash := plumbing.NewHash(sha)
	if _, err := r.CommitObject(hash); err != nil {
		return fmt.Errorf("base %s absent from clone: %w", sha, err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("arena-base"), hash)); err != nil {
		return err
	}
	endpoint, auth := c.arenaRepoEndpoint(org, name)
	remote, err := r.CreateRemoteAnonymous(&config.RemoteConfig{Name: "anonymous", URLs: []string{endpoint}})
	if err != nil {
		return err
	}
	return remote.PushContext(ctx, &gogit.PushOptions{RemoteName: "anonymous", RefSpecs: []config.RefSpec{"+refs/heads/arena-base:refs/heads/base"}, Auth: auth})
}

// BaseSHA reports the commit currently pinned as an arena repository's base.
// It uses the remote ref directly so it works for both the loom HTTP service
// and the file-backed repositories used by tests.
func (c ArenaClient) BaseSHA(org, name string) (string, error) {
	endpoint, auth := c.arenaRepoEndpoint(org, name)
	parsed, err := url.Parse(c.URL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		ctx := c.Context
		if ctx == nil {
			ctx = context.Background()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(name), nil)
		if err != nil {
			return "", err
		}
		req.SetBasicAuth(LoopbackUser, LoopbackPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return "", os.ErrNotExist
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("loom arena inspection: HTTP %d", resp.StatusCode)
		}
	}
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "arena", URLs: []string{endpoint}})
	refs, err := remote.List(&gogit.ListOptions{Auth: auth})
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.NewBranchReferenceName("base") {
			return ref.Hash().String(), nil
		}
	}
	return "", os.ErrNotExist
}

// Bundle fetches every branch and writes a Git v2 bundle with all fetched objects.
func (c ArenaClient) Bundle(org, name, path string) (err error) {
	dir, err := os.MkdirTemp("", "arena-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, auth := c.arenaRepoEndpoint(org, name)
	r, err := gogit.PlainCloneContext(ctx, dir, true, &gogit.CloneOptions{URL: endpoint, ReferenceName: plumbing.NewBranchReferenceName("base"), NoCheckout: true, Auth: auth})
	if err != nil {
		return err
	}
	refs, err := r.References()
	if err != nil {
		return err
	}
	defer refs.Close()
	branches := map[string]string{}
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			branches[ref.Name().Short()] = ref.Hash().String()
		}
		if ref.Name().IsRemote() && strings.HasPrefix(ref.Name().String(), "refs/remotes/origin/") && ref.Name().Short() != "origin/HEAD" {
			branches[strings.TrimPrefix(ref.Name().Short(), "origin/")] = ref.Hash().String()
		}
		return nil
	})
	if err != nil {
		return err
	}
	var lines []string
	for branch, hash := range branches {
		lines = append(lines, hash+" refs/heads/"+branch+"\n")
	}
	if len(lines) == 0 {
		return fmt.Errorf("arena repo %s/%s has no branches", org, name)
	}
	sort.Strings(lines)
	objects, err := r.Storer.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return err
	}
	defer objects.Close()
	var hashes []plumbing.Hash
	err = objects.ForEach(func(obj plumbing.EncodedObject) error { hashes = append(hashes, obj.Hash()); return nil })
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = io.WriteString(f, "# v2 git bundle\n"); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err = io.WriteString(f, line); err != nil {
			return err
		}
	}
	if _, err = io.WriteString(f, "\n"); err != nil {
		return err
	}
	_, err = packfile.NewEncoder(f, r.Storer, false).Encode(hashes, 0)
	return err
}

func (c ArenaClient) DeleteRepo(org, repo string) error {
	return c.request(http.MethodDelete, "/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo), nil, http.StatusNoContent, http.StatusNotFound)
}
func (c ArenaClient) DeleteOrg(org string) error {
	return c.request(http.MethodDelete, "/orgs/"+url.PathEscape(org), nil, http.StatusNoContent, http.StatusNotFound)
}
