package git

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// indexTree materialises the staged index as a tree object (what
// `git write-tree` does) so it can be diffed against another tree with
// the same machinery as two commits. Subtrees are written to the object
// store; unreachable until something references them, exactly like git.
func indexTree(repo *gogit.Repository) (*object.Tree, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	type node struct {
		files map[string]object.TreeEntry
		dirs  map[string]*node
	}
	newNode := func() *node {
		return &node{files: map[string]object.TreeEntry{}, dirs: map[string]*node{}}
	}
	root := newNode()
	for _, e := range idx.Entries {
		if e.Stage != 0 {
			return nil, fmt.Errorf("unmerged index entry %s", e.Name)
		}
		if e.Mode == filemode.Submodule {
			return nil, ErrUnsupported
		}
		parts := strings.Split(e.Name, "/")
		n := root
		for _, p := range parts[:len(parts)-1] {
			child, ok := n.dirs[p]
			if !ok {
				child = newNode()
				n.dirs[p] = child
			}
			n = child
		}
		base := parts[len(parts)-1]
		n.files[base] = object.TreeEntry{Name: base, Mode: e.Mode, Hash: e.Hash}
	}

	var write func(n *node) (plumbing.Hash, error)
	write = func(n *node) (plumbing.Hash, error) {
		entries := make([]object.TreeEntry, 0, len(n.files)+len(n.dirs))
		for _, f := range n.files {
			entries = append(entries, f)
		}
		for name, child := range n.dirs {
			h, err := write(child)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
		}
		// Git tree order compares names as if directories ended in "/".
		key := func(e object.TreeEntry) string {
			if e.Mode == filemode.Dir {
				return e.Name + "/"
			}
			return e.Name
		}
		sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
		t := &object.Tree{Entries: entries}
		eo := repo.Storer.NewEncodedObject()
		if err := t.Encode(eo); err != nil {
			return plumbing.ZeroHash, err
		}
		return repo.Storer.SetEncodedObject(eo)
	}
	h, err := write(root)
	if err != nil {
		return nil, err
	}
	return repo.TreeObject(h)
}

// emptyBlobHash is the id of the zero-byte blob (`git hash-object /dev/null`).
var emptyBlobHash = plumbing.NewHash("e69de29bb2d1d6434b8b29ae775ad8c2e48c5391")

var indexLineRE = regexp.MustCompile(`(?m)^index ([0-9a-f]{40})\.\.([0-9a-f]{40})`)

// stagedPatch renders `git diff --cached [REV] [-- paths]`: the index
// against REV's tree (HEAD's when rev is empty; an unborn HEAD diffs
// against the empty tree). A binary file in the patch returns
// ErrUnsupported — go-git cannot emit `GIT binary patch` hunks, and a
// "Binary files differ" stub is not appliable, so the caller must fall
// back to a host git rather than hand back a patch that silently drops
// the change.
func stagedPatch(repo *gogit.Repository, rev string, paths []string) (string, error) {
	base := &object.Tree{}
	if rev == "" {
		rev = "HEAD"
		if _, err := repo.Reference(plumbing.HEAD, true); err != nil {
			rev = ""
		}
	}
	if rev != "" {
		h, err := repo.ResolveRevision(plumbing.Revision(rev))
		if err != nil {
			return "", ErrUnsupported
		}
		c, err := repo.CommitObject(*h)
		if err != nil {
			return "", ErrUnsupported
		}
		if base, err = c.Tree(); err != nil {
			return "", ErrUnsupported
		}
	}
	idxTree, err := indexTree(repo)
	if err != nil {
		return "", ErrUnsupported
	}
	changes, err := object.DiffTree(base, idxTree)
	if err != nil {
		return "", ErrUnsupported
	}
	if len(paths) > 0 {
		kept := changes[:0:0]
		for _, c := range changes {
			name := c.To.Name
			if name == "" {
				name = c.From.Name
			}
			for _, p := range paths {
				if name == p || strings.HasPrefix(name, strings.TrimSuffix(p, "/")+"/") {
					kept = append(kept, c)
					break
				}
			}
		}
		changes = kept
	}
	if len(changes) == 0 {
		return "", nil
	}
	patch, err := changes.Patch()
	if err != nil {
		return "", ErrUnsupported
	}
	// go-git calls any patch without chunks binary, so a zero-byte file
	// added or deleted gets a "Binary files ... differ" stub; git prints
	// that entry header-only (mode + index lines, no ---/+++ or hunk).
	var stubs []string
	for _, fp := range patch.FilePatches() {
		if !fp.IsBinary() {
			continue
		}
		from, to := fp.Files()
		switch {
		case from == nil && to != nil && to.Hash() == emptyBlobHash:
			stubs = append(stubs, "Binary files /dev/null and b/"+to.Path()+" differ\n")
		case to == nil && from != nil && from.Hash() == emptyBlobHash:
			stubs = append(stubs, "Binary files a/"+from.Path()+" and /dev/null differ\n")
		default:
			return "", ErrUnsupported
		}
	}
	out := patch.String()
	for _, stub := range stubs {
		out = strings.Replace(out, stub, "", 1)
	}
	// go-git prints full 40-char blob ids; git abbreviates to 7.
	out = indexLineRE.ReplaceAllStringFunc(out, func(m string) string {
		sm := indexLineRE.FindStringSubmatch(m)
		return "index " + sm[1][:7] + ".." + sm[2][:7]
	})
	return out, nil
}
