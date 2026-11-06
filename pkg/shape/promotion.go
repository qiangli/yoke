package shape

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
}

type PushResult struct {
	Branch     string `json:"branch"`
	CompareURL string `json:"compare_url,omitempty"`
	PR         string `json:"pr,omitempty"`
}

func promotionRemote(ctx context.Context, repo *gogit.Repository, url, tier string) (plumbing.Hash, error) {
	name := "shape-promotion-" + tier
	remote := gogit.NewRemote(repo.Storer, &config.RemoteConfig{Name: name, URLs: []string{url}})
	ref := plumbing.ReferenceName("refs/shape-promotion/" + tier)
	err := remote.FetchContext(ctx, &gogit.FetchOptions{RemoteName: name, RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/HEAD:" + ref)}})
	if err != nil {
		// Most servers do not advertise HEAD as a branch. Resolve the advertised
		// default through a temporary clone when needed.
		tmp, e := os.MkdirTemp("", "shape-remote-*")
		if e != nil {
			return plumbing.ZeroHash, e
		}
		defer os.RemoveAll(tmp)
		clone, e := gogit.PlainCloneContext(ctx, tmp, true, &gogit.CloneOptions{URL: url})
		if e != nil {
			return plumbing.ZeroHash, e
		}
		head, e := clone.Head()
		if e != nil {
			return plumbing.ZeroHash, e
		}
		branch := head.Target().Short()
		if branch == "" {
			branch = "main"
		}
		err = remote.FetchContext(ctx, &gogit.FetchOptions{RemoteName: name, RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + branch + ":" + string(ref))}})
		if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return plumbing.ZeroHash, err
		}
	}
	r, e := repo.Reference(ref, true)
	if e != nil {
		return plumbing.ZeroHash, e
	}
	return r.Hash(), nil
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
func promoted(repo *gogit.Repository, head plumbing.Hash, wanted string) (bool, error) {
	seen := map[plumbing.Hash]bool{}
	queue := []plumbing.Hash{head}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if seen[h] {
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
			return nil, e
		}
		if v.App != s.App.Name || !v.Complete || v.Evidence == "" || v.ProposalRevision == "" {
			continue
		}
		if _, e = submittedItem(home, v); e != nil {
			return nil, e
		}
		base, e := repo.CommitObject(plumbing.NewHash(v.BaseRevision))
		if e != nil {
			return nil, e
		}
		candidate, e := repo.CommitObject(plumbing.NewHash(v.ProposalRevision))
		if e != nil {
			return nil, e
		}
		id, e := patchID(base, candidate)
		if e != nil {
			return nil, e
		}
		present, e := promoted(repo, head, id)
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
func ghArgs(v Submission) (string, []string) {
	title := "Submit shaped " + v.App
	by := v.Actor
	if v.RepresentedUser != "" && v.RepresentedUser != v.Actor {
		by += " for " + v.RepresentedUser
	}
	body := fmt.Sprintf("Shape: %s@%s\nShaped-By: %s\nEvidence: %s\n", v.App, v.SourceRevision, by, v.Evidence)
	return title, []string{"pr", "create", "--head", v.Branch, "--title", title, "--body", body}
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
	result := PushResult{Branch: v.Branch, CompareURL: strings.TrimSuffix(strings.TrimSuffix(url, ".git"), "/") + "/compare/" + v.Branch}
	if tier == "upstream" && createPR {
		if gh, e := exec.LookPath("gh"); e == nil {
			_, args := ghArgs(v)
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
	err := remote.PushContext(ctx, &gogit.PushOptions{RemoteName: "shape-push", RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return err
	}
	return nil
}
