package git

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// porcelainState is the corrected change model behind every native verb
// whose output callers diff to make decisions (status, ls-files, diff).
// It starts from worktreeStatus and repairs the shapes go-git's Status
// cannot express. Anything it cannot reproduce exactly as host git does
// is reported through unsupported, and the verb declines (ErrUnsupported)
// rather than print a plausible-looking wrong answer.
type porcelainState struct {
	idx       *index.Index
	tracked   map[string][2]byte // path → XY (index vs HEAD, worktree vs index)
	untracked []string           // every untracked, non-ignored file
	// unsupported names the first shape host git renders that this model
	// does not (merge conflicts, staged renames); empty when exact.
	unsupported string
}

func loadPorcelainState(repo *gogit.Repository, wt *gogit.Worktree) (*porcelainState, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	ps := &porcelainState{idx: idx, tracked: map[string][2]byte{}}
	for _, e := range idx.Entries {
		if e.Stage != 0 { // a merged entry is stage 0 (go-git's index.Merged is 1)
			ps.unsupported = "unmerged index entries"
			return ps, nil
		}
	}
	st, err := worktreeStatus(wt)
	if err != nil {
		return nil, err
	}
	head := headPaths(repo)
	inIndex := map[string]bool{}
	for _, e := range idx.Entries {
		inIndex[e.Name] = true
	}
	for path, fs := range st {
		if fs.Staging == gogit.Untracked && fs.Worktree == gogit.Untracked {
			// go-git folds "deleted from the index, still on disk" into
			// plain untracked; host git shows the staged delete as well.
			if head[path] && !inIndex[path] {
				ps.tracked[path] = [2]byte{'D', ' '}
			}
			ps.untracked = append(ps.untracked, path)
			continue
		}
		x, y := statusLetter(fs.Staging), statusLetter(fs.Worktree)
		if x != ' ' || y != ' ' {
			ps.tracked[path] = [2]byte{x, y}
		}
	}
	sort.Strings(ps.untracked)

	// Submodule content: " m" modified tracked content, " ?" untracked
	// content only. A moved pin (" M") is already in st.
	root := wt.Filesystem.Root()
	for _, e := range idx.Entries {
		if e.Mode != filemode.Submodule {
			continue
		}
		if xy, ok := ps.tracked[e.Name]; ok && xy[1] != ' ' {
			continue
		}
		if y := submoduleDirt(filepath.Join(root, filepath.FromSlash(e.Name))); y != ' ' {
			xy := ps.tracked[e.Name]
			if xy[0] == 0 {
				xy[0] = ' '
			}
			xy[1] = y
			ps.tracked[e.Name] = xy
		}
	}

	var added, deleted bool
	for _, xy := range ps.tracked {
		added = added || xy[0] == 'A'
		deleted = deleted || xy[0] == 'D'
	}
	if added && deleted {
		// Host git pairs similar ones into renames (status R, diff
		// R100); the native model does not draw them.
		var base *object.Tree
		if ref, err := repo.Head(); err == nil {
			if c, err := repo.CommitObject(ref.Hash()); err == nil {
				base, _ = c.Tree()
			}
		}
		if renamed, err := stagedRenames(repo, base); err != nil || renamed {
			ps.unsupported = unsupportedRenames
		}
	}
	return ps, nil
}

// unsupportedRenames is the reason a diff given --no-renames may ignore.
const unsupportedRenames = "staged renames"

func statusLetter(c gogit.StatusCode) byte {
	switch c {
	case gogit.Unmodified, gogit.Untracked:
		return ' '
	default:
		return byte(c)
	}
}

// headPaths is the set of paths (files and gitlinks) in HEAD's tree.
func headPaths(repo *gogit.Repository) map[string]bool {
	out := map[string]bool{}
	ref, err := repo.Head()
	if err != nil {
		return out
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return out
	}
	tree, err := c.Tree()
	if err != nil {
		return out
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		name, entry, err := w.Next()
		if err != nil {
			break
		}
		if entry.Mode != filemode.Dir {
			out[name] = true
		}
	}
	return out
}

// submoduleDirt reports 'm' (tracked content changed), '?' (only
// untracked content) or ' ' for the submodule checkout at dir.
func submoduleDirt(dir string) byte {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ' '
	}
	sub, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		return ' '
	}
	swt, err := sub.Worktree()
	if err != nil {
		return ' '
	}
	st, err := worktreeStatus(swt)
	if err != nil {
		return ' '
	}
	dirt := byte(' ')
	for _, fs := range st {
		if fs.Worktree == gogit.Untracked && fs.Staging == gogit.Untracked {
			dirt = '?'
			continue
		}
		if fs.Worktree != gogit.Unmodified || fs.Staging != gogit.Unmodified {
			return 'm'
		}
	}
	return dirt
}

// sortedTracked returns the tracked-change paths in byte order.
func (ps *porcelainState) sortedTracked() []string {
	out := make([]string, 0, len(ps.tracked))
	for p := range ps.tracked {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// collapsedUntracked applies host git's default -unormal rendering: an
// untracked file inside a directory that holds no tracked path is shown
// as that outermost directory ("dir/").
func (ps *porcelainState) collapsedUntracked() []string {
	indexDirs := map[string]bool{}
	for _, e := range ps.idx.Entries {
		for d := pathDir(e.Name); d != ""; d = pathDir(d) {
			indexDirs[d] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range ps.untracked {
		shown := p
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			d := strings.Join(parts[:i], "/")
			if !indexDirs[d] {
				shown = d + "/"
				break
			}
		}
		if !seen[shown] {
			seen[shown] = true
			out = append(out, shown)
		}
	}
	sort.Strings(out)
	return out
}

func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// quotePath renders a path the way host git does with core.quotePath
// on (the default): C-style quoting when the path holds a control
// character, a double quote, a backslash or a non-ASCII byte — and, for
// status output (sp), a space.
func quotePath(p string, sp bool) string {
	needs := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || c >= 0x80 || c == '"' || c == '\\' || (sp && c == ' ') {
			needs = true
			break
		}
	}
	if !needs {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c == 0x7f || c >= 0x80 {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
