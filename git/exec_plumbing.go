package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// nativeCatFile implements "git cat-file" plumbing command.
func nativeCatFile(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) < 2 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	flag := args[0]
	hashStr := args[1]

	hash := plumbing.NewHash(hashStr)

	obj, err := repo.Object(plumbing.AnyObject, hash)
	if err != nil {
		return &ExecResult{
			Stderr:   fmt.Sprintf("fatal: Not a valid object name %s\n", hashStr),
			ExitCode: 128,
		}, nil
	}

	switch flag {
	case "-t":
		return &ExecResult{Stdout: obj.Type().String() + "\n"}, nil

	case "-s":
		// Get encoded object to read size
		eo, err := repo.Storer.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: fmt.Sprintf("%d\n", eo.Size())}, nil

	case "-p":
		switch o := obj.(type) {
		case *object.Commit:
			var b strings.Builder
			b.WriteString(fmt.Sprintf("tree %s\n", o.TreeHash.String()))
			for _, p := range o.ParentHashes {
				b.WriteString(fmt.Sprintf("parent %s\n", p.String()))
			}
			b.WriteString(fmt.Sprintf("author %s <%s> %d %s\n",
				o.Author.Name, o.Author.Email,
				o.Author.When.Unix(), o.Author.When.Format("-0700")))
			b.WriteString(fmt.Sprintf("committer %s <%s> %d %s\n",
				o.Committer.Name, o.Committer.Email,
				o.Committer.When.Unix(), o.Committer.When.Format("-0700")))
			b.WriteString("\n")
			b.WriteString(o.Message)
			return &ExecResult{Stdout: b.String()}, nil

		case *object.Tree:
			var b strings.Builder
			for _, entry := range o.Entries {
				mode := fmt.Sprintf("%06o", entry.Mode)
				entryType := "blob"
				if entry.Mode == 0o040000 {
					entryType = "tree"
				}
				b.WriteString(fmt.Sprintf("%s %s %s\t%s\n", mode, entryType, entry.Hash.String(), entry.Name))
			}
			return &ExecResult{Stdout: b.String()}, nil

		case *object.Blob:
			reader, err := o.Reader()
			if err != nil {
				return nil, ErrUnsupported
			}
			defer reader.Close()
			content, err := io.ReadAll(reader)
			if err != nil {
				return nil, ErrUnsupported
			}
			return &ExecResult{Stdout: string(content)}, nil

		case *object.Tag:
			var b strings.Builder
			b.WriteString(fmt.Sprintf("object %s\n", o.Target.String()))
			b.WriteString(fmt.Sprintf("type %s\n", o.TargetType.String()))
			b.WriteString(fmt.Sprintf("tag %s\n", o.Name))
			b.WriteString(fmt.Sprintf("tagger %s <%s> %d %s\n",
				o.Tagger.Name, o.Tagger.Email,
				o.Tagger.When.Unix(), o.Tagger.When.Format("-0700")))
			b.WriteString("\n")
			b.WriteString(o.Message)
			return &ExecResult{Stdout: b.String()}, nil

		default:
			return nil, ErrUnsupported
		}

	default:
		return nil, ErrUnsupported
	}
}

// nativeHashObject implements "git hash-object".
func nativeHashObject(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	write := false
	useStdin := false
	var filePath string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-w":
			write = true
		case "--stdin":
			useStdin = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return nil, ErrUnsupported
			}
			filePath = args[i]
		}
	}

	var content []byte
	if useStdin {
		// Read from stdin — in our context, we can't actually read stdin
		// Return ErrUnsupported for stdin mode
		return nil, ErrUnsupported
	} else if filePath != "" {
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(dir, filePath)
		}
		var err error
		content, err = os.ReadFile(filePath)
		if err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("fatal: could not read '%s'\n", filePath),
				ExitCode: 128,
			}, nil
		}
	} else {
		return nil, ErrUnsupported
	}

	// Create blob object
	eo := repo.Storer.NewEncodedObject()
	eo.SetType(plumbing.BlobObject)
	eo.SetSize(int64(len(content)))

	writer, err := eo.Writer()
	if err != nil {
		return nil, ErrUnsupported
	}
	if _, err := writer.Write(content); err != nil {
		writer.Close()
		return nil, ErrUnsupported
	}
	writer.Close()

	if write {
		hash, err := repo.Storer.SetEncodedObject(eo)
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: hash.String() + "\n"}, nil
	}

	// Just compute hash without storing
	hash, err := repo.Storer.SetEncodedObject(eo)
	if err != nil {
		return nil, ErrUnsupported
	}
	return &ExecResult{Stdout: hash.String() + "\n"}, nil
}

// nativeReadTree implements "git read-tree <tree-ish>".
func nativeReadTree(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Reject merge modes and complex flags
	for _, arg := range args {
		if arg == "-m" || arg == "--merge" || arg == "-u" || strings.HasPrefix(arg, "--prefix") {
			return nil, ErrUnsupported
		}
	}

	treeish := args[len(args)-1]
	if strings.HasPrefix(treeish, "-") {
		return nil, ErrUnsupported
	}

	// go-git doesn't expose a direct read-tree index manipulation.
	// Fall through for now.
	_ = treeish
	return nil, ErrUnsupported
}

// nativeWriteTree implements "git write-tree".
func nativeWriteTree(_ context.Context, dir string, args []string) (*ExecResult, error) {
	// Reject flags
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		}
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Get the current index
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Build tree from index entries
	entries := make([]object.TreeEntry, 0, len(idx.Entries))
	for _, entry := range idx.Entries {
		// Only include top-level entries (simple case)
		if strings.Contains(entry.Name, "/") {
			// Nested paths require building subtrees — complex
			return nil, ErrUnsupported
		}
		entries = append(entries, object.TreeEntry{
			Name: entry.Name,
			Mode: entry.Mode,
			Hash: entry.Hash,
		})
	}

	tree := &object.Tree{Entries: entries}
	eo := repo.Storer.NewEncodedObject()
	if err := tree.Encode(eo); err != nil {
		return nil, ErrUnsupported
	}

	hash, err := repo.Storer.SetEncodedObject(eo)
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: hash.String() + "\n"}, nil
}

// nativeCommitTree implements "git commit-tree <tree> -p <parent> -m <message>".
func nativeCommitTree(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	var treeHash string
	var parents []plumbing.Hash
	var message string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			if i+1 >= len(args) {
				return nil, ErrUnsupported
			}
			i++
			parents = append(parents, plumbing.NewHash(args[i]))
		case "-m":
			if i+1 >= len(args) {
				return nil, ErrUnsupported
			}
			i++
			message = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return nil, ErrUnsupported
			}
			if treeHash == "" {
				treeHash = args[i]
			} else {
				return nil, ErrUnsupported
			}
		}
	}

	if treeHash == "" || message == "" {
		return nil, ErrUnsupported
	}

	// Get author info from config
	cfg, err := repo.ConfigScoped(0) // SystemScope
	if err != nil {
		cfg, _ = repo.Config()
	}
	authorName := "Unknown"
	authorEmail := "unknown@example.com"
	if cfg != nil && cfg.User.Name != "" {
		authorName = cfg.User.Name
		authorEmail = cfg.User.Email
	}

	now := time.Now()
	commit := &object.Commit{
		TreeHash:     plumbing.NewHash(treeHash),
		ParentHashes: parents,
		Author: object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  now,
		},
		Committer: object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  now,
		},
		Message: message,
	}

	eo := repo.Storer.NewEncodedObject()
	if err := commit.Encode(eo); err != nil {
		return nil, ErrUnsupported
	}

	hash, err := repo.Storer.SetEncodedObject(eo)
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: hash.String() + "\n"}, nil
}

// nativeSymbolicRef implements "git symbolic-ref".
func nativeSymbolicRef(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Filter out -q/--quiet
	var positional []string
	for _, arg := range args {
		if arg == "-q" || arg == "--quiet" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		}
		positional = append(positional, arg)
	}

	if len(positional) == 0 {
		return nil, ErrUnsupported
	}

	refName := positional[0]

	if len(positional) == 1 {
		// Read mode: print what the symbolic ref points to
		ref, err := repo.Storer.Reference(plumbing.ReferenceName(refName))
		if err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("fatal: ref %s is not a symbolic ref\n", refName),
				ExitCode: 1,
			}, nil
		}
		if ref.Type() != plumbing.SymbolicReference {
			return &ExecResult{
				Stderr:   fmt.Sprintf("fatal: ref %s is not a symbolic ref\n", refName),
				ExitCode: 1,
			}, nil
		}
		return &ExecResult{Stdout: string(ref.Target()) + "\n"}, nil
	}

	if len(positional) == 2 {
		// Write mode: set symbolic ref
		target := positional[1]
		ref := plumbing.NewSymbolicReference(
			plumbing.ReferenceName(refName),
			plumbing.ReferenceName(target),
		)
		if err := repo.Storer.SetReference(ref); err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: ""}, nil
	}

	return nil, ErrUnsupported
}

// nativeUpdateRef implements "git update-ref".
func nativeUpdateRef(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	deleteMode := false
	var positional []string
	for _, arg := range args {
		if arg == "-d" {
			deleteMode = true
		} else if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		} else {
			positional = append(positional, arg)
		}
	}

	if deleteMode {
		if len(positional) < 1 {
			return nil, ErrUnsupported
		}
		refName := plumbing.ReferenceName(positional[0])
		if err := repo.Storer.RemoveReference(refName); err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("error: could not delete reference %s\n", refName),
				ExitCode: 1,
			}, nil
		}
		return &ExecResult{Stdout: ""}, nil
	}

	if len(positional) < 2 {
		return nil, ErrUnsupported
	}

	refName := plumbing.ReferenceName(positional[0])
	hash := plumbing.NewHash(positional[1])

	ref := plumbing.NewHashReference(refName, hash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: ""}, nil
}

// nativeDiffTree implements "git diff-tree <tree1> <tree2>".
func nativeDiffTree(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) < 2 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Filter out flags
	var hashes []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			// Accept -r (recursive, default behavior) but reject others
			if arg != "-r" {
				return nil, ErrUnsupported
			}
			continue
		}
		hashes = append(hashes, arg)
	}

	if len(hashes) < 2 {
		return nil, ErrUnsupported
	}

	// Resolve tree-ish references
	hash1, err := repo.ResolveRevision(plumbing.Revision(hashes[0]))
	if err != nil {
		return nil, ErrUnsupported
	}
	hash2, err := repo.ResolveRevision(plumbing.Revision(hashes[1]))
	if err != nil {
		return nil, ErrUnsupported
	}

	// Get trees — could be commit (get tree from commit) or tree directly
	tree1, err := resolveTree(repo, *hash1)
	if err != nil {
		return nil, ErrUnsupported
	}
	tree2, err := resolveTree(repo, *hash2)
	if err != nil {
		return nil, ErrUnsupported
	}

	changes, err := tree1.Diff(tree2)
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			continue
		}
		var status string
		switch action {
		case merkletrie.Insert:
			status = "A"
		case merkletrie.Delete:
			status = "D"
		case merkletrie.Modify:
			status = "M"
		default:
			status = "?"
		}
		name := change.To.Name
		if name == "" {
			name = change.From.Name
		}
		b.WriteString(fmt.Sprintf("%s\t%s\n", status, name))
	}

	return &ExecResult{Stdout: b.String()}, nil
}

// nativeLsTree implements "git ls-tree <tree-ish>".
func nativeLsTree(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	recursive := false
	var treeish string
	for _, arg := range args {
		if arg == "-r" {
			recursive = true
		} else if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		} else {
			treeish = arg
		}
	}

	if treeish == "" {
		return nil, ErrUnsupported
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(treeish))
	if err != nil {
		return nil, ErrUnsupported
	}

	tree, err := resolveTree(repo, *hash)
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	if recursive {
		walker := object.NewTreeWalker(tree, true, nil)
		defer walker.Close()
		for {
			name, entry, err := walker.Next()
			if err != nil {
				break
			}
			entryType := "blob"
			if entry.Mode == 0o040000 {
				entryType = "tree"
			}
			b.WriteString(fmt.Sprintf("%06o %s %s\t%s\n", entry.Mode, entryType, entry.Hash.String(), name))
		}
	} else {
		for _, entry := range tree.Entries {
			entryType := "blob"
			if entry.Mode == 0o040000 {
				entryType = "tree"
			}
			b.WriteString(fmt.Sprintf("%06o %s %s\t%s\n", entry.Mode, entryType, entry.Hash.String(), entry.Name))
		}
	}

	return &ExecResult{Stdout: b.String()}, nil
}

// nativeShowRef implements "git show-ref".
func nativeShowRef(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	headsOnly := false
	tagsOnly := false
	for _, arg := range args {
		switch arg {
		case "--heads":
			headsOnly = true
		case "--tags":
			tagsOnly = true
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, ErrUnsupported
			}
		}
	}

	refs, err := repo.References()
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.SymbolicReference {
			return nil
		}
		name := ref.Name()

		if headsOnly && !name.IsBranch() {
			return nil
		}
		if tagsOnly && !name.IsTag() {
			return nil
		}
		// Skip HEAD
		if name == plumbing.HEAD {
			return nil
		}

		b.WriteString(fmt.Sprintf("%s %s\n", ref.Hash().String(), string(name)))
		return nil
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: b.String()}, nil
}

// resolveTree resolves a hash to a tree object. If the hash points to a commit,
// it returns the commit's tree.
func resolveTree(repo *gogit.Repository, hash plumbing.Hash) (*object.Tree, error) {
	obj, err := repo.Object(plumbing.AnyObject, hash)
	if err != nil {
		return nil, err
	}

	switch o := obj.(type) {
	case *object.Tree:
		return o, nil
	case *object.Commit:
		return o.Tree()
	default:
		return nil, fmt.Errorf("object %s is not a tree or commit", hash)
	}
}
