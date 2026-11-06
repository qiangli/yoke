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
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
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
