package shape

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/mod/semver"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/src"
)

// OutgoingChange is a verified submission absent from a promotion tier.
type OutgoingChange struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Base      string            `json:"base"`
	Candidate string            `json:"candidate"`
	Evidence  string            `json:"evidence_path"`
	Status    map[string]string `json:"status"`
	Warning   string            `json:"warning,omitempty"`
}

type PushResult struct {
	Branch     string `json:"branch"`
	CompareURL string `json:"compare_url,omitempty"`
	PR         string `json:"pr,omitempty"`
}

type IncomingChange struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Ref     string `json:"ref"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Warning string `json:"warning,omitempty"`
}

type PullResult struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

func promotionAuth(ctx context.Context, raw string) (transport.AuthMethod, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, err
	}
	if u.User != nil {
		password, _ := u.User.Password()
		return &http.BasicAuth{Username: u.User.Username(), Password: password}, nil
	}
	if token := os.Getenv("GH_TOKEN"); token != "" {
		return &http.BasicAuth{Username: "x-access-token", Password: token}, nil
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return &http.BasicAuth{Username: "x-access-token", Password: token}, nil
	}
	git := "git"
	if _, err := os.Stat("/usr/bin/git"); err == nil {
		git = "/usr/bin/git"
	} else if found, err := exec.LookPath("git"); err == nil {
		git = found
	} else {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, git, "credential", "fill")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	cmd.Stdin = strings.NewReader("protocol=" + u.Scheme + "\nhost=" + u.Host + "\npath=" + strings.TrimPrefix(u.Path, "/") + "\n\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, nil
	}
	credentials := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			credentials[k] = v
		}
	}
	if credentials["username"] == "" || credentials["password"] == "" {
		return nil, nil
	}
	return &http.BasicAuth{Username: credentials["username"], Password: credentials["password"]}, nil
}
func promotionRemote(ctx context.Context, repo *gogit.Repository, raw, tier string) (plumbing.Hash, error) {
	name := "shape-promotion-" + tier
	auth, err := promotionAuth(ctx, raw)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	remote := gogit.NewRemote(repo.Storer, &config.RemoteConfig{Name: name, URLs: []string{raw}})
	refs, err := remote.List(&gogit.ListOptions{Auth: auth})
	if err != nil {
		return plumbing.ZeroHash, err
	}
	var branch string
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			branch = r.Target().String()
			break
		}
	}
	if branch == "" {
		return plumbing.ZeroHash, fmt.Errorf("shape: remote %s has no symbolic HEAD", tier)
	}
	ref := plumbing.ReferenceName("refs/shape-promotion/" + tier)
	err = remote.FetchContext(ctx, &gogit.FetchOptions{RemoteName: name, Auth: auth, RefSpecs: []config.RefSpec{config.RefSpec("+" + branch + ":" + ref.String())}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return plumbing.ZeroHash, err
	}
	fetched, err := repo.Reference(ref, true)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return fetched.Hash(), nil
}

func patchID(base, candidate *object.Commit) (string, error) {
	patch, err := base.Patch(candidate)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, line := range strings.Split(patch.String(), "\n") {
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "\\ No newline") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
				lines = append(lines, line)
				continue
			}
			lines = append(lines, strings.Join(strings.Fields(line), ""))
		} else if strings.HasPrefix(line, "diff --git ") {
			lines = append(lines, line)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}
func promoted(repo *gogit.Repository, head, base plumbing.Hash, wanted string) (bool, error) {
	remote, err := repo.CommitObject(head)
	if err != nil {
		return false, err
	}
	proposalBase, err := repo.CommitObject(base)
	if err != nil {
		return false, err
	}
	common, err := remote.MergeBase(proposalBase)
	if err != nil {
		return false, err
	}
	stop := map[plumbing.Hash]bool{}
	for _, c := range common {
		stop[c.Hash] = true
	}
	seen := map[plumbing.Hash]bool{}
	queue := []plumbing.Hash{head}
	for len(queue) > 0 {
		if len(seen) >= 512 {
			return false, fmt.Errorf("shape: patch comparison exceeds 512 commits")
		}
		h := queue[0]
		queue = queue[1:]
		if seen[h] || stop[h] {
			continue
		}
		seen[h] = true
		c, err := repo.CommitObject(h)
		if err != nil {
			return false, err
		}
		if len(c.ParentHashes) > 0 {
			p, err := repo.CommitObject(c.ParentHashes[0])
			if err != nil {
				return false, err
			}
			id, err := patchID(p, c)
			if err != nil {
				return false, err
			}
			if id == wanted {
				return true, nil
			}
		}
		queue = append(queue, c.ParentHashes...)
	}
	return false, nil
}

func (s Service) incomingRemote(ctx context.Context, tier string) (*gogit.Repository, *gogit.Remote, []*plumbing.Reference, plumbing.Hash, error) {
	home, err := s.home()
	if err != nil {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	raw, err := src.ConfiguredTier(home, s.App.Name, tier)
	if err != nil {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	if raw == "" {
		return nil, nil, nil, plumbing.ZeroHash, fmt.Errorf("shape: tier %s is not configured", tier)
	}
	repo, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	auth, err := promotionAuth(ctx, raw)
	if err != nil {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	remote := gogit.NewRemote(repo.Storer, &config.RemoteConfig{Name: "shape-incoming", URLs: []string{raw}})
	refs, err := remote.List(&gogit.ListOptions{Auth: auth})
	if err != nil {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	var branch plumbing.ReferenceName
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			branch = r.Target()
			break
		}
	}
	if branch == "" {
		return nil, nil, nil, plumbing.ZeroHash, fmt.Errorf("shape: remote %s has no symbolic HEAD", tier)
	}
	var specs []config.RefSpec
	for _, r := range refs {
		if r.Name() == branch || strings.HasPrefix(r.Name().String(), "refs/heads/shape-submit/") || strings.HasPrefix(r.Name().String(), "refs/tags/") {
			specs = append(specs, config.RefSpec("+"+r.Name().String()+":refs/shape-incoming/"+tier+"/"+strings.TrimPrefix(r.Name().String(), "refs/")))
		}
	}
	err = remote.FetchContext(ctx, &gogit.FetchOptions{RemoteName: "shape-incoming", Auth: auth, RefSpecs: specs, Tags: gogit.NoTags})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	base, err := repo.Head()
	if err != nil {
		return nil, nil, nil, plumbing.ZeroHash, err
	}
	return repo, remote, refs, base.Hash(), nil
}

func (s Service) Incoming(ctx context.Context, tier string) ([]IncomingChange, error) {
	repo, _, refs, base, err := s.incomingRemote(ctx, tier)
	if err != nil {
		return nil, err
	}
	out := []IncomingChange{}
	var defaultRef plumbing.ReferenceName
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			defaultRef = r.Target()
		}
	}
	for _, r := range refs {
		name := r.Name().String()
		if strings.HasPrefix(name, "refs/heads/shape-submit/") {
			id := strings.TrimPrefix(name, "refs/heads/shape-submit/")
			commit, e := repo.CommitObject(r.Hash())
			if e != nil {
				return nil, e
			}
			if len(commit.ParentHashes) == 0 {
				continue
			}
			parent, e := repo.CommitObject(commit.ParentHashes[0])
			if e != nil {
				return nil, e
			}
			patch, e := patchID(parent, commit)
			if e != nil {
				return nil, e
			}
			present, e := promoted(repo, base, parent.Hash, patch)
			if e != nil {
				return nil, e
			}
			if !present {
				out = append(out, IncomingChange{Kind: "change", ID: id, Ref: name, Title: strings.Split(commit.Message, "\n")[0], Status: "incoming"})
			}
		} else if r.Name() == defaultRef {
			count, titles := 0, []string{}
			seen := map[plumbing.Hash]bool{}
			queue := []plumbing.Hash{r.Hash()}
			for len(queue) > 0 && count < 512 {
				h := queue[0]
				queue = queue[1:]
				if h == base || seen[h] {
					continue
				}
				seen[h] = true
				c, e := repo.CommitObject(h)
				if e != nil {
					return nil, e
				}
				local, e := repo.CommitObject(base)
				if e != nil {
					return nil, e
				}
				common, e := local.MergeBase(c)
				if e != nil {
					return nil, e
				}
				shared := false
				for _, ancestor := range common {
					if ancestor.Hash == h {
						shared = true
						break
					}
				}
				if shared {
					continue
				}
				count++
				if len(titles) < 3 {
					titles = append(titles, strings.Split(c.Message, "\n")[0])
				}
				queue = append(queue, c.ParentHashes...)
			}
			if count > 0 {
				out = append(out, IncomingChange{Kind: "commits", ID: fmt.Sprint(count), Ref: name, Title: strings.Join(titles, "; "), Status: "incoming"})
			}
		} else if strings.HasPrefix(name, "refs/tags/") {
			tag := strings.TrimPrefix(name, "refs/tags/")
			if semver.IsValid(tag) && semver.IsValid(s.Options.InstalledVersion) && semver.Compare(tag, s.Options.InstalledVersion) > 0 {
				out = append(out, IncomingChange{Kind: "release", ID: tag, Ref: name, Title: tag, Status: "incoming"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].Ref < out[j].Ref
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

func (s Service) Pull(ctx context.Context, tier, ref string) (PullResult, error) {
	if !strings.HasPrefix(ref, "refs/heads/shape-submit/") && !strings.HasPrefix(ref, "refs/tags/") {
		return PullResult{}, fmt.Errorf("shape: unsupported incoming ref %q", ref)
	}
	_, _, refs, _, err := s.incomingRemote(ctx, tier)
	if err != nil {
		return PullResult{}, err
	}
	var hash plumbing.Hash
	for _, r := range refs {
		if r.Name().String() == ref {
			hash = r.Hash()
			break
		}
	}
	if hash == plumbing.ZeroHash {
		return PullResult{}, fmt.Errorf("shape: unknown incoming ref %q", ref)
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return PullResult{Kind: "release", Tag: strings.TrimPrefix(ref, "refs/tags/")}, nil
	}
	root, err := s.root()
	if err != nil {
		return PullResult{}, err
	}
	dir, err := os.MkdirTemp(root, "incoming-")
	if err != nil {
		if err = os.MkdirAll(root, 0700); err != nil {
			return PullResult{}, err
		}
		dir, err = os.MkdirTemp(root, "incoming-")
		if err != nil {
			return PullResult{}, err
		}
	}
	checkout := filepath.Join(dir, "work")
	{
		clone, e := gogit.PlainInit(checkout, false)
		if e != nil {
			_ = os.RemoveAll(dir)
			return PullResult{}, e
		}
		remote := gogit.NewRemote(clone.Storer, &config.RemoteConfig{Name: "source", URLs: []string{s.App.SourceDir}})
		private := "refs/shape-incoming/" + tier + "/" + strings.TrimPrefix(ref, "refs/")
		e = remote.FetchContext(ctx, &gogit.FetchOptions{RemoteName: "source", RefSpecs: []config.RefSpec{config.RefSpec("+" + private + ":refs/heads/incoming")}})
		if e != nil && !errors.Is(e, gogit.NoErrAlreadyUpToDate) {
			_ = os.RemoveAll(dir)
			return PullResult{}, e
		}
		wt, e := clone.Worktree()
		if e == nil {
			e = wt.Checkout(&gogit.CheckoutOptions{Hash: hash, Force: true})
		}
		if e != nil {
			_ = os.RemoveAll(dir)
			return PullResult{}, e
		}
	}
	return PullResult{Kind: "change", Path: checkout}, nil
}
func (s Service) Outgoing(ctx context.Context, tier string) ([]OutgoingChange, error) {
	home, err := s.home()
	if err != nil {
		return nil, err
	}
	url, err := src.ConfiguredTier(home, s.App.Name, tier)
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, fmt.Errorf("shape: tier %s is not configured", tier)
	}
	repo, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		return nil, err
	}
	head, err := promotionRemote(ctx, repo, url, tier)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(submissionRoot(home))
	if os.IsNotExist(err) {
		return []OutgoingChange{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []OutgoingChange{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var v Submission
		b, e := os.ReadFile(filepath.Join(submissionRoot(home), entry.Name()))
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			out = append(out, OutgoingChange{ID: strings.TrimSuffix(entry.Name(), ".json"), Status: map[string]string{tier: "warning"}, Warning: e.Error()})
			continue
		}
		if v.App != s.App.Name || !v.Complete || v.Evidence == "" || v.ProposalRevision == "" {
			continue
		}
		if _, e = submittedItem(home, v); e != nil {
			out = append(out, OutgoingChange{ID: v.ID, Base: v.BaseRevision, Candidate: v.ProposalRevision, Evidence: v.Evidence, Status: map[string]string{tier: "warning"}, Warning: e.Error()})
			continue
		}
		base, e := repo.CommitObject(plumbing.NewHash(v.BaseRevision))
		if e != nil {
			out = append(out, OutgoingChange{ID: v.ID, Status: map[string]string{tier: "warning"}, Warning: e.Error()})
			continue
		}
		candidate, e := repo.CommitObject(plumbing.NewHash(v.ProposalRevision))
		if e != nil {
			out = append(out, OutgoingChange{ID: v.ID, Base: v.BaseRevision, Candidate: v.ProposalRevision, Evidence: v.Evidence, Status: map[string]string{tier: "warning"}, Warning: e.Error()})
			continue
		}
		id, e := patchID(base, candidate)
		if e != nil {
			out = append(out, OutgoingChange{ID: v.ID, Base: v.BaseRevision, Candidate: v.ProposalRevision, Evidence: v.Evidence, Status: map[string]string{tier: "warning"}, Warning: e.Error()})
			continue
		}
		present, e := promoted(repo, head, base.Hash, id)
		if e != nil {
			return nil, e
		}
		if present {
			continue
		}
		out = append(out, OutgoingChange{ID: v.ID, Title: strings.Split(candidate.Message, "\n")[0], Base: v.BaseRevision, Candidate: v.ProposalRevision, Evidence: v.Evidence, Status: map[string]string{tier: "outgoing"}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func ghArgs(v Submission, repo string) (string, []string) {
	title := "Submit shaped " + v.App
	by := v.Actor
	if v.RepresentedUser != "" && v.RepresentedUser != v.Actor {
		by += " for " + v.RepresentedUser
	}
	body := fmt.Sprintf("Shape: %s@%s\nShaped-By: %s\nEvidence: %s\n", v.App, v.SourceRevision, by, v.Evidence)
	return title, []string{"pr", "create", "--repo", repo, "--head", v.Branch, "--title", title, "--body", body}
}
func (s Service) Push(ctx context.Context, tier, id string, createPR bool) (PushResult, error) {
	home, err := s.home()
	if err != nil {
		return PushResult{}, err
	}
	url, err := src.ConfiguredTier(home, s.App.Name, tier)
	if err != nil {
		return PushResult{}, err
	}
	explicit, err := src.ConfiguredUpstream(home, s.App.Name)
	if err != nil {
		return PushResult{}, err
	}
	if tier == "upstream" && explicit == "" {
		return PushResult{}, errors.New("shape: configure push destination with shape remote set upstream <url>")
	}
	if url == "" {
		return PushResult{}, fmt.Errorf("shape: tier %s is not configured", tier)
	}
	v, err := loadSubmission(home, id)
	if err != nil {
		return PushResult{}, err
	}
	if v.App != s.App.Name || !v.Complete || v.Evidence == "" {
		return PushResult{}, errors.New("shape: submission is not verified")
	}
	if _, err = submittedItem(home, v); err != nil {
		return PushResult{}, err
	}
	repo, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		return PushResult{}, err
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(v.Branch), true)
	if err != nil {
		return PushResult{}, err
	}
	if ref.Hash().String() != v.ProposalRevision {
		return PushResult{}, errors.New("shape: proposal branch changed")
	}
	err = pushBranch(ctx, repo, url, v.Branch)
	if err != nil {
		return PushResult{}, err
	}
	result := PushResult{Branch: v.Branch, CompareURL: compareURL(url, v.Branch)}
	if tier == "upstream" && createPR {
		if gh, e := exec.LookPath("gh"); e == nil {
			repoName := repositoryName(url)
			if repoName == "" {
				return result, errors.New("shape: upstream has no GitHub repository name")
			}
			_, args := ghArgs(v, repoName)
			cmd := exec.CommandContext(ctx, gh, args...)
			cmd.Dir = s.App.SourceDir
			output, e := cmd.CombinedOutput()
			if e != nil {
				return result, fmt.Errorf("shape: gh pr create: %w: %s", e, strings.TrimSpace(string(output)))
			}
			result.PR = strings.TrimSpace(string(output))
		}
	}
	return result, nil
}
func pushBranch(ctx context.Context, repo *gogit.Repository, url, branch string) error {
	remote := gogit.NewRemote(repo.Storer, &config.RemoteConfig{Name: "shape-push", URLs: []string{url}})
	auth, err := promotionAuth(ctx, url)
	if err != nil {
		return err
	}
	err = remote.PushContext(ctx, &gogit.PushOptions{Auth: auth, RemoteName: "shape-push", RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return err
	}
	return nil
}

func repositoryName(raw string) string {
	if strings.HasPrefix(raw, "git@") {
		_, path, ok := strings.Cut(raw, ":")
		if !ok {
			return ""
		}
		return strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
}
func compareURL(raw, branch string) string {
	if strings.HasPrefix(raw, "git@") {
		host, path, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if !ok || host == "" {
			return ""
		}
		return "https://" + host + "/" + strings.TrimSuffix(strings.Trim(path, "/"), ".git") + "/compare/" + branch
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	u.User = nil
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git") + "/compare/" + branch
	return u.String()
}
