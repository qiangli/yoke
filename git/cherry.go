package git

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// nativeCherry implements "git cherry [--abbrev[=n]] [-v] <upstream> [<head>]"
// via go-git. It reports, for each commit reachable from <head> but not from
// <upstream>, whether its change is already present upstream under a possibly
// different SHA ("-" = equivalent, "+" = missing) — the branch-cleanup
// predicate ("is this branch's work already in master?").
//
// Equivalence is patch-identity, mirroring host git as probed 2026-10-08:
//
//   - merge commits are never listed (host git skips them silently);
//   - listing order is oldest-first;
//   - the upstream equivalence set is the non-merge commits in
//     merge-base..upstream (post-merge-base only: an equivalent commit that
//     predates the merge-base still reports "+", matching host git);
//   - default output is "<sign> <full-sha>"; -v appends the subject;
//     --abbrev[=n] shortens SHAs (default 7).
//
// The identity hash itself is an internal detail (commit SHAs are what get
// printed): per-file normalized blocks (paths + op lines, line numbers
// excluded, hunks order-independent) folded with SHA-256. Any other flag,
// zero or 3+ positionals, unresolvable revisions, or unrelated histories
// return ErrUnsupported — never an approximation.
func nativeCherry(_ context.Context, dir string, args []string) (*ExecResult, error) {
	verbose := false
	abbrev := -1 // -1 = full SHA
	var positionals []string
	for _, a := range args {
		switch {
		case a == "-v":
			verbose = true
		case a == "--abbrev":
			abbrev = 7
		case strings.HasPrefix(a, "--abbrev="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--abbrev="))
			if err != nil || n <= 0 {
				return nil, ErrUnsupported
			}
			abbrev = n
		case strings.HasPrefix(a, "-"):
			return nil, ErrUnsupported
		default:
			positionals = append(positionals, a)
		}
	}
	if len(positionals) < 1 || len(positionals) > 2 {
		return nil, ErrUnsupported
	}
	upstreamRev, headRev := positionals[0], "HEAD"
	if len(positionals) == 2 {
		headRev = positionals[1]
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	upstream, err := resolveRevisionCommit(repo, upstreamRev)
	if err != nil {
		return nil, ErrUnsupported
	}
	head, err := resolveRevisionCommit(repo, headRev)
	if err != nil {
		return nil, ErrUnsupported
	}
	bases, err := upstream.MergeBase(head)
	if err != nil || len(bases) == 0 {
		return nil, ErrUnsupported
	}
	mb := bases[0].Hash

	upstreamIDs, err := rangePatchIDs(repo, mb, upstream.Hash)
	if err != nil {
		return nil, ErrUnsupported
	}
	headCommits, err := rangeCommits(repo, mb, head.Hash)
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	for _, c := range headCommits {
		if c.NumParents() != 1 && c.NumParents() != 0 {
			continue // merges are never listed
		}
		id, err := patchIdentity(repo, c)
		if err != nil {
			return nil, ErrUnsupported
		}
		sign := "+"
		if upstreamIDs[id] {
			sign = "-"
		}
		sha := c.Hash.String()
		if abbrev >= 0 && abbrev < len(sha) {
			sha = sha[:abbrev]
		}
		b.WriteString(sign + " " + sha)
		if verbose {
			subj, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
			b.WriteString(" " + subj)
		}
		b.WriteString("\n")
	}
	return &ExecResult{Stdout: b.String()}, nil
}

// rangePatchIDs returns the patch-identity set of the non-merge commits
// reachable from tip but not from exclude (the merge-base..tip range).
func rangePatchIDs(repo *gogit.Repository, exclude, tip plumbing.Hash) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range walkRange(repo, exclude, tip) {
		if c.NumParents() != 1 && c.NumParents() != 0 {
			continue
		}
		id, err := patchIdentity(repo, c)
		if err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, nil
}

// rangeCommits returns the non-merge commits reachable from tip but not from
// exclude, oldest-first.
func rangeCommits(repo *gogit.Repository, exclude, tip plumbing.Hash) ([]*object.Commit, error) {
	all := walkRange(repo, exclude, tip)
	var out []*object.Commit
	for _, c := range all {
		if c.NumParents() == 1 || c.NumParents() == 0 {
			out = append(out, c)
		}
	}
	// go-git Log yields newest-first; cherry lists oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// walkRange lists commits reachable from tip excluding exclude and its
// ancestors, newest-first (go-git Log order).
func walkRange(repo *gogit.Repository, exclude, tip plumbing.Hash) []*object.Commit {
	excluded := map[plumbing.Hash]bool{}
	if it, err := repo.Log(&gogit.LogOptions{From: exclude}); err == nil {
		_ = it.ForEach(func(c *object.Commit) error {
			excluded[c.Hash] = true
			return nil
		})
		it.Close()
	}
	var out []*object.Commit
	if it, err := repo.Log(&gogit.LogOptions{From: tip}); err == nil {
		_ = it.ForEach(func(c *object.Commit) error {
			if !excluded[c.Hash] {
				out = append(out, c)
			}
			return nil
		})
		it.Close()
	}
	return out
}

// patchIdentity computes a content-based identity for a single-parent (or
// root) commit: same change under a different SHA yields the same identity.
// One normalized block per file (paths + op lines, hunk order-independent),
// folded with SHA-256. Binary files fold their blob hashes.
func patchIdentity(repo *gogit.Repository, c *object.Commit) (string, error) {
	var blocks []string
	if c.NumParents() == 0 {
		tree, err := c.Tree()
		if err != nil {
			return "", err
		}
		var paths []string
		files := map[string]string{}
		err = tree.Files().ForEach(func(f *object.File) error {
			contents, err := f.Contents()
			if err != nil {
				return err
			}
			paths = append(paths, f.Name)
			files[f.Name] = contents
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(paths)
		for _, p := range paths {
			var b strings.Builder
			b.WriteString("file \x00" + p + " \x00" + p + "\n")
			for _, line := range strings.Split(files[p], "\n") {
				b.WriteString("+ " + line + "\n")
			}
			blocks = append(blocks, fmt.Sprintf("%x", sha256.Sum256([]byte(b.String()))))
		}
	} else {
		parent, err := c.Parent(0)
		if err != nil {
			return "", err
		}
		patch, err := parent.Patch(c)
		if err != nil {
			return "", err
		}
		for _, fp := range patch.FilePatches() {
			from, to := fp.Files()
			fromPath, toPath := "", ""
			fromHash, toHash := plumbing.ZeroHash, plumbing.ZeroHash
			if from != nil {
				fromPath, fromHash = from.Path(), from.Hash()
			}
			if to != nil {
				toPath, toHash = to.Path(), to.Hash()
			}
			var b strings.Builder
			b.WriteString("file \x00" + fromPath + " \x00" + toPath + "\n")
			if fp.IsBinary() {
				b.WriteString(fmt.Sprintf("binary %s %s\n", fromHash, toHash))
			} else {
				for _, ch := range fp.Chunks() {
					var op byte
					switch ch.Type() {
					case diff.Equal:
						op = ' '
					case diff.Add:
						op = '+'
					case diff.Delete:
						op = '-'
					default:
						return "", fmt.Errorf("cherry: unknown chunk op")
					}
					for _, line := range strings.Split(ch.Content(), "\n") {
						b.WriteString(string([]byte{op}) + " " + line + "\n")
					}
				}
			}
			blocks = append(blocks, fmt.Sprintf("%x", sha256.Sum256([]byte(b.String()))))
		}
	}
	sort.Strings(blocks)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(blocks, "\n")))), nil
}

// resolveRevisionCommit resolves rev to a commit object.
func resolveRevisionCommit(repo *gogit.Repository, rev string) (*object.Commit, error) {
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(*h)
}
