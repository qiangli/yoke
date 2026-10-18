package git

import (
	"errors"
	"fmt"
	"path"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// OpenRepository opens the repository at or above dir exactly as the argv
// handlers do: linked worktrees (commondir) and alternates resolve.
func OpenRepository(dir string) (*gogit.Repository, error) { return openRepo(dir) }

// RevPath is git's `<rev>:<path>` resolved natively.
type RevPath struct {
	Rev    string            // the revision as spelled
	Path   string            // root-relative path; "" is the root tree
	Commit *object.Commit    // nil when Rev names a tree directly
	Tree   plumbing.Hash     // root tree the path was looked up in
	Hash   plumbing.Hash     // the addressed object
	Mode   filemode.FileMode // filemode.Dir for trees (the root included)
}

// ErrRevPathForm marks rev:path spellings with no native meaning: the index
// forms (`:path`, `:N:path`, `:/text`) and cwd-relative `./`/`../` paths,
// which git resolves against the caller's directory.
var ErrRevPathForm = errors.New("git: rev:path form not served natively")

// ErrPathNotInRev is returned when the path does not exist in the revision.
var ErrPathNotInRev = errors.New("path does not exist in revision")

// SplitRevPath splits `<rev>:<path>` at the first colon (a refname cannot
// contain one). ok is false when spec has no colon.
func SplitRevPath(spec string) (rev, p string, ok bool) {
	return strings.Cut(spec, ":")
}

// ResolveRevPath resolves rev to a tree-ish (peeling tags and commits) and
// looks p up in it. p is root-relative, as in `git show HEAD:pkg/x.go`.
func ResolveRevPath(repo *gogit.Repository, rev, p string) (*RevPath, error) {
	if rev == "" || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == ".." {
		return nil, ErrRevPathForm
	}
	// A full object id may name a tree or tag, which ResolveRevision
	// (commit-ish only) would refuse.
	h := plumbing.NewHash(rev)
	if len(rev) != 40 || !plumbing.IsHash(rev) {
		r, err := repo.ResolveRevision(plumbing.Revision(rev))
		if err != nil {
			return nil, err
		}
		h = *r
	}
	rp := &RevPath{Rev: rev}
	tree, err := peelToTree(repo, h, rp)
	if err != nil {
		return nil, err
	}
	rp.Tree = tree.Hash
	clean := strings.Trim(p, "/")
	if clean != "" {
		clean = path.Clean(clean)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, ErrRevPathForm
		}
	}
	rp.Path = clean
	if clean == "" {
		rp.Hash, rp.Mode = tree.Hash, filemode.Dir
		return rp, nil
	}
	e, err := tree.FindEntry(clean)
	if err != nil {
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, fmt.Errorf("%w: path '%s' does not exist in '%s'", ErrPathNotInRev, clean, rev)
		}
		return nil, err
	}
	rp.Hash, rp.Mode = e.Hash, e.Mode
	return rp, nil
}

func peelToTree(repo *gogit.Repository, h plumbing.Hash, rp *RevPath) (*object.Tree, error) {
	for i := 0; i < 16; i++ {
		obj, err := repo.Object(plumbing.AnyObject, h)
		if err != nil {
			return nil, err
		}
		switch o := obj.(type) {
		case *object.Tag:
			h = o.Target
		case *object.Commit:
			rp.Commit = o
			return o.Tree()
		case *object.Tree:
			return o, nil
		default:
			return nil, fmt.Errorf("git: %s is not a tree-ish", rp.Rev)
		}
	}
	return nil, fmt.Errorf("git: %s: tag chain too deep", rp.Rev)
}
