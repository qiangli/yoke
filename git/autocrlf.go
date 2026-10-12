package git

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// go-git implements no core.autocrlf. Git for Windows ships
// core.autocrlf=true system-wide, so without this a CRLF checkout reads
// as modified whenever go-git's stat shortcut misses, and a native add
// stores CRLF blobs host git would have stored as LF. This file
// reproduces git's convert_to_git for core.autocrlf=true|input (no
// gitattributes text/eol rules — those decline, see eolAttributesSet).

// coreAutoCRLF resolves core.autocrlf with git's precedence — local,
// then global ($XDG_CONFIG_HOME/git/config, ~/.gitconfig), then system
// (unless GIT_CONFIG_NOSYSTEM) — normalized to "true", "input" or "false".
func coreAutoCRLF(repo *gogit.Repository) string {
	if cfg, err := repo.Config(); err == nil {
		if v := cfg.Raw.Section("core").Option("autocrlf"); v != "" {
			return autocrlfValue(v)
		}
	}
	for _, p := range gitConfigFiles() {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		c := config.NewConfig()
		if c.Unmarshal(data) != nil {
			continue
		}
		if v := c.Raw.Section("core").Option("autocrlf"); v != "" {
			return autocrlfValue(v)
		}
	}
	return "false"
}

func autocrlfValue(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return "true"
	case "input":
		return "input"
	}
	return "false"
}

// gitConfigFiles lists the global then system config files, highest
// precedence first.
func gitConfigFiles() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".gitconfig"))
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		out = append(out, filepath.Join(xdg, "git", "config"))
	}
	if os.Getenv("GIT_CONFIG_NOSYSTEM") != "" {
		return out
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
			if d := os.Getenv(env); d != "" {
				out = append(out, filepath.Join(d, "Git", "etc", "gitconfig"))
			}
		}
		if d := os.Getenv("ProgramData"); d != "" {
			out = append(out, filepath.Join(d, "Git", "config"))
		}
		return out
	}
	return append(out, "/etc/gitconfig")
}

// crlfStats mirrors git's gather_stats / convert_is_binary.
type crlfStats struct {
	nul, loneCR, crlf, printable, nonprintable int
}

func gatherCRLFStats(b []byte) crlfStats {
	var s crlfStats
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\r' {
			if i+1 < len(b) && b[i+1] == '\n' {
				s.crlf++
				i++
			} else {
				s.loneCR++
			}
			continue
		}
		switch {
		case c == '\n':
		case c == 127:
			s.nonprintable++
		case c < 32:
			switch c {
			case '\b', '\t', '\033', '\014':
				s.printable++
			case 0:
				s.nul++
			default:
				s.nonprintable++
			}
		default:
			s.printable++
		}
	}
	if len(b) > 0 && b[len(b)-1] == '\032' {
		s.nonprintable-- // DOS EOF marker
	}
	return s
}

func (s crlfStats) binary() bool {
	return s.loneCR > 0 || s.nul > 0 || (s.printable>>7) < s.nonprintable
}

// toGit applies core.autocrlf's CRLF→LF conversion to worktree bytes.
// indexHasCR is git's has_crlf_in_index guard: a path whose current
// blob already holds CR is never converted. ok is false when nothing
// changes.
func toGit(content []byte, indexHasCR bool) ([]byte, bool) {
	if indexHasCR || !bytes.Contains(content, []byte("\r\n")) {
		return nil, false
	}
	if gatherCRLFStats(content).binary() {
		return nil, false
	}
	return bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")), true
}

func blobHasCR(repo *gogit.Repository, h plumbing.Hash) bool {
	if h.IsZero() {
		return false
	}
	b, err := repo.BlobObject(h)
	if err != nil {
		return false
	}
	r, err := b.Reader()
	if err != nil {
		return false
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	return err == nil && bytes.IndexByte(data, '\r') >= 0
}

// eolAttributesSet reports whether a gitattributes file at the root or in
// $GIT_DIR/info sets text/eol/crlf rules, which override core.autocrlf
// and are not reproduced natively.
func eolAttributesSet(root string) bool {
	files := []string{filepath.Join(root, ".gitattributes")}
	if gitDir, err := resolveGitDir(root); err == nil {
		files = append(files, filepath.Join(gitDir, "info", "attributes"))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := string(data)
		if strings.Contains(s, "text") || strings.Contains(s, "eol") || strings.Contains(s, "crlf") {
			return true
		}
	}
	return false
}

// dropAutoCRLFClean clears the worktree-modified mark of files that equal
// their index blob once converted the way host git converts them.
func dropAutoCRLFClean(repo *gogit.Repository, root string, st gogit.Status) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return
	}
	entries := map[string]plumbing.Hash{}
	for _, e := range idx.Entries {
		if e.Mode == filemode.Regular || e.Mode == filemode.Executable {
			entries[e.Name] = e.Hash
		}
	}
	for path, fs := range st {
		if fs.Worktree != gogit.Modified {
			continue
		}
		h, ok := entries[path]
		if !ok {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			continue
		}
		conv, changed := toGit(content, blobHasCR(repo, h))
		if !changed || plumbing.ComputeHash(plumbing.BlobObject, conv) != h {
			continue
		}
		fs.Worktree = gogit.Unmodified
		if fs.Staging == gogit.Unmodified {
			delete(st, path)
		}
	}
}

// indexSnapshot records each index path's blob before an add.
func indexSnapshot(repo *gogit.Repository) map[string]plumbing.Hash {
	snap := map[string]plumbing.Hash{}
	if idx, err := repo.Storer.Index(); err == nil {
		for _, e := range idx.Entries {
			snap[e.Name] = e.Hash
		}
	}
	return snap
}

// applyAutoCRLFToAdded rewrites the blobs a go-git add just staged
// (entries whose hash differs from before) with core.autocrlf's
// conversion, as host git's add would have stored them.
func applyAutoCRLFToAdded(repo *gogit.Repository, root string, before map[string]plumbing.Hash) error {
	if mode := coreAutoCRLF(repo); mode != "true" && mode != "input" {
		return nil
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	changed := false
	for _, e := range idx.Entries {
		prev, existed := before[e.Name]
		if (existed && prev == e.Hash) || (e.Mode != filemode.Regular && e.Mode != filemode.Executable) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Name)))
		if err != nil {
			continue
		}
		conv, ok := toGit(content, existed && blobHasCR(repo, prev))
		if !ok {
			continue
		}
		obj := repo.Storer.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		obj.SetSize(int64(len(conv)))
		w, err := obj.Writer()
		if err != nil {
			return err
		}
		if _, err := w.Write(conv); err != nil {
			w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		h, err := repo.Storer.SetEncodedObject(obj)
		if err != nil {
			return err
		}
		e.Hash = h // Size stays the worktree file's stat size, as git records it
		changed = true
	}
	if !changed {
		return nil
	}
	return repo.Storer.SetIndex(idx)
}
