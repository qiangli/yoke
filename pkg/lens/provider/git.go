package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/ref"
)

// Git serves `git:REV:PATH` from the object store of the repository at or
// above Dir: no checkout, no worktree read. Each revision resolves once per
// provider, so one walk sees one snapshot even if a branch moves under it.
// `git:REV:a.zip!/x` descends into a blob with the archive rules.
type Git struct {
	Dir    string
	Limits Limits

	mu     sync.Mutex
	repo   *gogit.Repository
	revs   map[string]plumbing.Hash
	chains chainCache
}

func NewGit(dir string) *Git {
	if dir == "" {
		dir = "."
	}
	return &Git{Dir: dir, Limits: DefaultLimits}
}

func (g *Git) resolve(ctx context.Context, loc ref.Locator) (*gogit.Repository, *ygit.RevPath, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if loc.Scheme != "git" || loc.ScopeKind != "" || loc.Revision == "" {
		return nil, nil, ErrUnsupported
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.repo == nil {
		r, err := ygit.OpenRepository(g.Dir)
		if err != nil {
			return nil, nil, fmt.Errorf("provider: not a git repository: %w", err)
		}
		g.repo, g.revs = r, map[string]plumbing.Hash{}
	}
	pinned, ok := g.revs[loc.Revision]
	if !ok {
		h, err := g.repo.ResolveRevision(plumbing.Revision(loc.Revision))
		if err != nil {
			return nil, nil, fmt.Errorf("provider: git revision %q: %w", loc.Revision, err)
		}
		pinned = *h
		g.revs[loc.Revision] = pinned
	}
	rp, err := ygit.ResolveRevPath(g.repo, pinned.String(), loc.Target)
	if err != nil {
		if errors.Is(err, ygit.ErrPathNotInRev) {
			return nil, nil, &fs.PathError{Op: "stat", Path: loc.String(), Err: fs.ErrNotExist}
		}
		return nil, nil, err
	}
	rp.Rev = loc.Revision
	return g.repo, rp, nil
}

// inner resolves `!/` descent from the addressed blob.
func (g *Git) inner(ctx context.Context, repo *gogit.Repository, rp *ygit.RevPath, loc ref.Locator) (*archiveFS, string, error) {
	if !rp.Mode.IsFile() {
		return nil, "", fmt.Errorf("provider: %s: cannot descend into a non-file", loc.Target)
	}
	format := ContainerFormat(rp.Path)
	if format == "" {
		return nil, "", fmt.Errorf("provider: %s: not a container (known suffixes: .zip .jar .tar .tgz .tar.gz .gz)", rp.Path)
	}
	key := "git:" + rp.Hash.String()
	// The blob is buffered (bounded) only when its container is first indexed.
	src := blob{name: path.Base(rp.Path)}
	b, err := repo.BlobObject(rp.Hash)
	if err != nil {
		return nil, "", err
	}
	if b.Size > g.Limits.MaxBuffer {
		return nil, "", fmt.Errorf("provider: %s: blob exceeds the %d byte buffer limit", rp.Path, g.Limits.MaxBuffer)
	}
	var data []byte
	load := func() error {
		if data != nil {
			return nil
		}
		r, err := b.Reader()
		if err != nil {
			return err
		}
		defer r.Close()
		data, err = io.ReadAll(io.LimitReader(r, g.Limits.MaxBuffer))
		return err
	}
	src.size = b.Size
	src.open = func() (io.ReadCloser, error) {
		if err := load(); err != nil {
			return nil, err
		}
		return bytesBlob(src.name, data).open()
	}
	src.at = func() (io.ReaderAt, io.Closer, error) {
		if err := load(); err != nil {
			return nil, nil, err
		}
		return bytesBlob(src.name, data).at()
	}
	return g.chains.descend(ctx, g.Limits, key, src, format, loc.Inner)
}

func gitKind(m filemode.FileMode) (kind, typ string) {
	switch m {
	case filemode.Dir:
		return "directory", "inode/directory"
	case filemode.Symlink:
		return "symlink", "inode/symlink"
	case filemode.Submodule:
		return "submodule", "application/x-git-submodule"
	}
	return "file", "application/octet-stream"
}

func (g *Git) node(repo *gogit.Repository, rp *ygit.RevPath, loc ref.Locator) (Node, error) {
	kind, typ := gitKind(rp.Mode)
	n := Node{SchemaVersion: NodeSchema, Ref: loc.String(), Path: rp.Path, Name: path.Base(rp.Path), Kind: kind, Type: typ,
		Mode: fmt.Sprintf("%06o", uint32(rp.Mode)), Attrs: map[string]any{"object": rp.Hash.String()}}
	if rp.Commit != nil {
		n.Rev = rp.Commit.Hash.String()
	} else {
		n.Rev = rp.Tree.String()
	}
	if rp.Path == "" {
		n.Name = rp.Rev
		n.Attrs["tree"] = rp.Tree.String()
		if c := rp.Commit; c != nil {
			n.Attrs["commit"] = c.Hash.String()
			n.Attrs["author"] = c.Author.Name + " <" + c.Author.Email + ">"
			n.Attrs["committer"] = c.Committer.Name + " <" + c.Committer.Email + ">"
			n.Attrs["subject"] = subject(c.Message)
			parents := make([]string, 0, len(c.ParentHashes))
			for _, p := range c.ParentHashes {
				parents = append(parents, p.String())
			}
			n.Attrs["parents"] = parents
			n.Mtime = c.Committer.When.UTC().Format(time.RFC3339Nano)
		}
	}
	switch kind {
	case "directory":
		t, err := repo.TreeObject(rp.Hash)
		if err != nil {
			return Node{}, err
		}
		c := uint64(len(t.Entries))
		n.Children = &c
	case "file", "symlink":
		size, err := blobSize(repo, rp.Hash)
		if err != nil {
			return Node{}, err
		}
		n.Size = &size
		if c := ContainerFormat(rp.Path); c != "" && kind == "file" {
			n.Attrs["container"] = c
		}
	}
	return n, nil
}

func subject(msg string) string {
	for i := 0; i < len(msg); i++ {
		if msg[i] == '\n' {
			return msg[:i]
		}
	}
	return msg
}

// blobSize reads the object header, not the content, when the store can.
func blobSize(repo *gogit.Repository, h plumbing.Hash) (int64, error) {
	if s, ok := repo.Storer.(interface {
		EncodedObjectSize(plumbing.Hash) (int64, error)
	}); ok {
		if n, err := s.EncodedObjectSize(h); err == nil {
			return n, nil
		}
	}
	b, err := repo.BlobObject(h)
	if err != nil {
		return 0, err
	}
	return b.Size, nil
}

func (g *Git) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	repo, rp, err := g.resolve(ctx, loc)
	if err != nil {
		return Node{}, err
	}
	if len(loc.Inner) > 0 {
		fsys, p, err := g.inner(ctx, repo, rp, loc)
		if err != nil {
			return Node{}, err
		}
		return fsys.node(loc, p)
	}
	return g.node(repo, rp, loc)
}

func (g *Git) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	repo, rp, err := g.resolve(ctx, loc)
	if err != nil {
		return nil, err
	}
	if len(loc.Inner) > 0 {
		fsys, p, err := g.inner(ctx, repo, rp, loc)
		if err != nil {
			return nil, err
		}
		return fsys.list(ctx, loc, p)
	}
	if rp.Mode != filemode.Dir {
		return nil, &fs.PathError{Op: "list", Path: loc.String(), Err: errors.New("not a tree")}
	}
	t, err := repo.TreeObject(rp.Hash)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(t.Entries))
	for _, e := range t.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cl := loc
		cl.Target = path.Join(rp.Path, e.Name)
		child := *rp
		child.Path, child.Hash, child.Mode = cl.Target, e.Hash, e.Mode
		n, err := g.node(repo, &child, cl)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (g *Git) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	repo, rp, err := g.resolve(ctx, loc)
	if err != nil {
		return nil, err
	}
	if len(loc.Inner) > 0 {
		fsys, p, err := g.inner(ctx, repo, rp, loc)
		if err != nil {
			return nil, err
		}
		return fsys.openMember(p)
	}
	if !rp.Mode.IsFile() {
		kind, _ := gitKind(rp.Mode)
		return nil, &fs.PathError{Op: "open", Path: loc.String(), Err: fmt.Errorf("%s has no blob content", kind)}
	}
	b, err := repo.BlobObject(rp.Hash)
	if err != nil {
		return nil, err
	}
	r, err := b.Reader()
	if err != nil {
		return nil, err
	}
	return &limitedReader{rc: r, max: g.Limits.MaxMember}, nil
}

// Edges are tree containment, plus `parent` from a commit root to each
// parent commit's root — the history edge an agent follows to step back.
func (g *Git) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := g.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	edges := containsEdges(loc, children)
	if len(loc.Inner) > 0 || loc.Target != "" {
		return edges, nil
	}
	_, rp, err := g.resolve(ctx, loc)
	if err != nil || rp.Commit == nil {
		return edges, err
	}
	for _, p := range rp.Commit.ParentHashes {
		dst := ref.Locator{Scheme: "git", Revision: p.String()}
		edges = append(edges, Edge{SchemaVersion: EdgeSchema, Src: loc.String(), Rel: "parent", Dst: dst.String(), Attrs: map[string]any{}})
	}
	return edges, nil
}

var _ Provider = (*Git)(nil)
var _ Provider = (*Archive)(nil)
var _ fs.ReadDirFS = (*archiveFS)(nil)
var _ fs.StatFS = (*archiveFS)(nil)
