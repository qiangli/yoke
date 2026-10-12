package git

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// indexTree materialises the staged index as a tree object (what
// `git write-tree` does) so it can be diffed against another tree with
// the same machinery as two commits. The trees live in an in-memory
// overlay over the repository's objects, so a read verb (status,
// diff --cached) never writes into the repository.
func indexTree(repo *gogit.Repository) (*object.Tree, error) {
	ov := &overlayStorer{EncodedObjectStorer: repo.Storer, mem: memory.NewStorage()}
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
		eo := ov.NewEncodedObject()
		if err := t.Encode(eo); err != nil {
			return plumbing.ZeroHash, err
		}
		return ov.SetEncodedObject(eo)
	}
	h, err := write(root)
	if err != nil {
		return nil, err
	}
	return object.GetTree(ov, h)
}

// overlayStorer reads through to the repository's objects and keeps
// writes in memory.
type overlayStorer struct {
	storer.EncodedObjectStorer
	mem *memory.Storage
}

func (o *overlayStorer) NewEncodedObject() plumbing.EncodedObject { return o.mem.NewEncodedObject() }

func (o *overlayStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	return o.mem.SetEncodedObject(obj)
}

func (o *overlayStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if obj, err := o.mem.EncodedObject(t, h); err == nil {
		return obj, nil
	}
	return o.EncodedObjectStorer.EncodedObject(t, h)
}

func (o *overlayStorer) HasEncodedObject(h plumbing.Hash) error {
	if o.mem.HasEncodedObject(h) == nil {
		return nil
	}
	return o.EncodedObjectStorer.HasEncodedObject(h)
}

// stagedRenames reports whether host git would pair any staged deletion
// with a staged addition (diff.renames on, 50% similarity): a shape the
// native renderers do not draw, so they decline.
func stagedRenames(repo *gogit.Repository, base *object.Tree) (bool, error) {
	idxTree, err := indexTree(repo)
	if err != nil {
		return false, err
	}
	return treeRenames(base, idxTree)
}

// treeRenames reports whether host git's rename detection (50%
// similarity) pairs any deletion in base→target with an addition.
func treeRenames(base, target *object.Tree) (bool, error) {
	if base == nil {
		base = &object.Tree{}
	}
	changes, err := object.DiffTreeWithOptions(context.Background(), base, target,
		&object.DiffTreeOptions{DetectRenames: true, RenameScore: 50})
	if err != nil {
		return false, err
	}
	for _, c := range changes {
		if c.From.Name != "" && c.To.Name != "" && c.From.Name != c.To.Name {
			return true, nil
		}
	}
	return false, nil
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
func stagedPatch(repo *gogit.Repository, rev string, paths []string, noRenames bool) (string, error) {
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
	return treePatch(base, idxTree, paths, noRenames)
}

// treePatch renders `git diff base target` the way host git prints it
// (abbreviated index ids, header-only empty files, TAB after spaced
// names), declining (ErrUnsupported) on what it cannot match: binary
// hunks, C-quoted names, and — unless noRenames — any pair host git's
// rename detection would join.
func treePatch(base, target *object.Tree, paths []string, noRenames bool) (string, error) {
	changes, err := object.DiffTree(base, target)
	if err != nil {
		return "", ErrUnsupported
	}
	if !noRenames {
		if renamed, err := treeRenames(base, target); err != nil || renamed {
			return "", ErrUnsupported
		}
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
	for _, c := range changes {
		// git C-quotes such names in every header line; go-git does not.
		if quotePath(c.From.Name, false) != c.From.Name || quotePath(c.To.Name, false) != c.To.Name {
			return "", ErrUnsupported
		}
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
	// git ends a ---/+++ name that holds a space with a TAB, so the
	// name's end stays unambiguous.
	// Only header lines (between "diff --git" and the first "@@") count.
	lines := strings.SplitAfter(out, "\n")
	inHeader := false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inHeader = true
		case strings.HasPrefix(l, "@@"):
			inHeader = false
		case inHeader && (strings.HasPrefix(l, "--- a/") || strings.HasPrefix(l, "+++ b/")) && strings.Contains(l[6:], " "):
			lines[i] = strings.TrimSuffix(l, "\n") + "\t\n"
		}
	}
	out = strings.Join(lines, "")
	// go-git prints full 40-char blob ids; git abbreviates to 7.
	out = indexLineRE.ReplaceAllStringFunc(out, func(m string) string {
		sm := indexLineRE.FindStringSubmatch(m)
		return "index " + sm[1][:7] + ".." + sm[2][:7]
	})
	return out, nil
}
