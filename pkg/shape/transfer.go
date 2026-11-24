package shape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/coreutils/pkg/patch"
	"github.com/qiangli/yoke/pkg/shape/appserver"
)

const maxTransferBytes = 8 << 20

type SourceFile struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Content []byte `json:"content"` // JSON base64, including binary files and symlink targets
}

type SourceSnapshot struct {
	Revision string       `json:"revision"`
	Files    []SourceFile `json:"files"`
}

func (s Service) allowSourceTransfer(c Caller) error {
	if err := s.validate(c); err != nil {
		return err
	}
	if s.Kind() != "web" || s.isBashy() || s.Owner() == "" || isStrangerCaller(c) {
		return ErrUnauthorized
	}
	switch c.Tier {
	case "owner":
		if s.isOwner(c) {
			return nil
		}
	case "member":
		scopes, err := s.Scopes()
		if err != nil {
			return err
		}
		if scopes.AdmitsMember(appserver.ScopeShape, c.Circle, c.Groups, callerEmail(c)) {
			return nil
		}
	}
	return ErrUnauthorized
}

// PullSource reads tracked blobs from a single immutable commit on the owner's line.
func (s Service) PullSource(ctx context.Context, caller Caller) (SourceSnapshot, error) {
	if err := s.allowSourceTransfer(caller); err != nil {
		return SourceSnapshot{}, err
	}
	rev, err := s.active(s.Owner())
	if err != nil {
		return SourceSnapshot{}, err
	}
	r, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		return SourceSnapshot{}, err
	}
	commit, err := r.CommitObject(plumbing.NewHash(rev))
	if err != nil {
		return SourceSnapshot{}, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return SourceSnapshot{}, err
	}
	out := SourceSnapshot{Revision: rev, Files: []SourceFile{}}
	remaining := int64(maxTransferBytes)
	walk := object.NewTreeWalker(tree, true, nil)
	defer walk.Close()
	for {
		if err := ctx.Err(); err != nil {
			return SourceSnapshot{}, err
		}
		name, entry, err := walk.Next()
		if err == io.EOF {
			if name != "" {
				return SourceSnapshot{}, fmt.Errorf("shape: incomplete source tree at %q", name)
			}
			return out, nil
		}
		if err != nil {
			return SourceSnapshot{}, err
		}
		if entry.Mode == filemode.Dir {
			continue
		}
		if entry.Mode == filemode.Submodule {
			return SourceSnapshot{}, fmt.Errorf("shape: cannot export submodule %q", name)
		}
		blob, err := r.BlobObject(entry.Hash)
		if err != nil {
			return SourceSnapshot{}, err
		}
		// Charge paths and per-file metadata too, so empty files are bounded.
		remaining -= int64(len(name) + 128)
		if blob.Size > remaining {
			return SourceSnapshot{}, errors.New("shape: active source exceeds the 8 MiB transfer limit")
		}
		reader, err := blob.Reader()
		if err != nil {
			return SourceSnapshot{}, err
		}
		body, err := io.ReadAll(io.LimitReader(reader, blob.Size+1))
		reader.Close()
		if err != nil || int64(len(body)) != blob.Size {
			return SourceSnapshot{}, fmt.Errorf("shape: cannot read source blob for %q", name)
		}
		remaining -= blob.Size
		out.Files = append(out.Files, SourceFile{Path: name, Mode: entry.Mode.String(), Content: body})
	}
}

// PushPatch imports a unified diff against an explicit owner revision, then runs the
// same verifier as shape verify. Approve performs its own locked base check.
func (s Service) PushPatch(ctx context.Context, caller Caller, base, diff string) (Candidate, error) {
	if err := s.allowSourceTransfer(caller); err != nil {
		return Candidate{}, err
	}
	if base == "" || diff == "" || len(diff) > maxTransferBytes {
		return Candidate{}, errors.New("shape: base_revision and a non-empty unified diff up to 8 MiB are required")
	}
	c, err := s.importChanges(ctx, caller, Wish{Request: "Import member patch"}, base, func(dir string) error {
		return applySourcePatch(dir, diff)
	})
	if err != nil {
		return c, err
	}
	return s.Verify(ctx, caller, c.ID)
}

func applySourcePatch(dir, diff string) error {
	// The shared parser accepts POSIX patch input and normalizes CRLF.
	// For a Git diff with LF control lines, a CR before LF is blob data.
	preserveCR := false
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@ ") && !strings.HasSuffix(line, "\r"):
			preserveCR = true
		case strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "),
			strings.HasPrefix(line, "new file mode ") && strings.TrimSuffix(line, "\r") != "new file mode 100644":
			return errors.New("shape: only unified text changes to regular files are supported")
		}
	}
	if preserveCR {
		diff = strings.ReplaceAll(diff, "\r\n", "\r\r\n")
	}
	p, err := patch.Parse([]byte(diff))
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	gitDir, err := root.Stat(".git")
	if err != nil {
		return err
	}
	for _, f := range p.Files {
		if f.Format != patch.FormatUnified || f.Unsupported != "" || f.RenameFrom != "" || f.RenameTo != "" || len(f.Hunks) == 0 {
			return errors.New("shape: expected unified text hunks")
		}
		old, newName := f.OldName, f.NewName
		if (strings.HasPrefix(old, "a/") || f.IsCreate()) && (strings.HasPrefix(newName, "b/") || f.IsDelete()) {
			old, newName = strings.TrimPrefix(old, "a/"), strings.TrimPrefix(newName, "b/")
		}
		name := newName
		if f.IsDelete() {
			name = old
		} else if !f.IsCreate() && old != newName {
			return errors.New("shape: use delete and create hunks to rename a file")
		}
		if err := applySourceFile(root, gitDir, name, f); err != nil {
			return fmt.Errorf("shape: patch %q: %w", name, err)
		}
	}
	return nil
}

func applySourceFile(root *os.Root, gitDir fs.FileInfo, name string, f patch.FilePatch) error {
	if !fs.ValidPath(name) || name == "." || !filepath.IsLocal(name) || strings.ContainsAny(name, "\\:") {
		return errors.New("expected a workspace-relative file")
	}
	// Reject symlinks throughout the path and filesystem aliases for .git.
	// The workspace is private to this import until its candidate is ready.
	var rel string
	var info fs.FileInfo
	for part := range strings.SplitSeq(name, "/") {
		if strings.EqualFold(part, ".git") {
			return errors.New("path enters repository metadata")
		}
		rel = filepath.Join(rel, part)
		entry, err := root.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if entry.Mode()&fs.ModeSymlink != 0 || os.SameFile(entry, gitDir) {
			return errors.New("path enters a symlink or repository metadata")
		}
		if rel == filepath.FromSlash(name) {
			info = entry
		}
	}
	var original string
	if f.IsCreate() {
		if info != nil {
			return fs.ErrExist
		}
	} else {
		if info == nil || !info.Mode().IsRegular() || info.Size() > maxTransferBytes {
			return errors.New("expected a regular source file up to 8 MiB")
		}
		b, err := root.ReadFile(rel)
		if err != nil {
			return err
		}
		original = string(b)
	}
	body, err := applySourceHunks(original, f.Hunks)
	if err != nil {
		return err
	}
	if f.IsDelete() {
		if body != "" {
			return errors.New("delete hunk leaves content")
		}
		return root.Remove(name)
	}
	if f.IsCreate() {
		if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
	}
	return root.WriteFile(name, []byte(body), 0644)
}

// A push names an exact base. Match at the declared positions, including
// final-newline markers, without fuzzy searches or partially applied hunks.
func applySourceHunks(original string, hunks []patch.Hunk) (string, error) {
	var out strings.Builder
	oldLine, newLine := 0, 0
	for _, h := range hunks {
		if h.OldStart < oldLine || h.NewStart-newLine != h.OldStart-oldLine {
			return "", errors.New("invalid hunk position")
		}
		for oldLine < h.OldStart {
			if original == "" {
				return "", errors.New("hunk starts past end of file")
			}
			line, rest, newline := strings.Cut(original, "\n")
			out.WriteString(line)
			if newline {
				out.WriteByte('\n')
			}
			original = rest
			oldLine++
			newLine++
		}
		for _, line := range h.Lines {
			text := line.Text
			if !line.NoEOL {
				text += "\n"
			}
			if line.Kind != patch.LineAdd {
				var ok bool
				if original, ok = strings.CutPrefix(original, text); !ok {
					return "", errors.New("hunk does not match base content")
				}
				if line.NoEOL && original != "" {
					return "", errors.New("hunk expects end of file")
				}
				oldLine++
			}
			if line.Kind != patch.LineDelete {
				out.WriteString(text)
				newLine++
			}
		}
		if oldLine-h.OldStart != h.OldCount || newLine-h.NewStart != h.NewCount {
			return "", errors.New("invalid hunk counts")
		}
	}
	out.WriteString(original)
	return out.String(), nil
}
