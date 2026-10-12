package git

import (
	"bytes"
	"github.com/go-git/go-billy/v5"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitattributes"
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
	if v, ok := configValue(repo, "core", "autocrlf"); ok {
		return autocrlfValue(v)
	}
	return "false"
}

// configValue resolves section.name with git's precedence — local, then
// global, then system (gitConfigFiles). A key present with an empty
// value reports ok with "".
func configValue(repo *gogit.Repository, section, name string) (string, bool) {
	lookup := func(c *config.Config) (string, bool) {
		sec := c.Raw.Section(section)
		if !sec.HasOption(name) {
			return "", false
		}
		return sec.Option(name), true
	}
	if cfg, err := repo.Config(); err == nil {
		if v, ok := lookup(cfg); ok {
			return v, true
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
		if v, ok := lookup(c); ok {
			return v, true
		}
	}
	return "", false
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

// eolRules matches gitattributes text/eol/crlf rules against paths;
// such rules override core.autocrlf and are not reproduced natively. As in
// git, only the .gitattributes files of a path's ancestor directories
// (plus $GIT_DIR/info/attributes) apply, and they are read on demand —
// never by walking the tree.
type eolRules struct {
	root string
	fs   billy.Filesystem
	dirs map[string][]gitattributes.MatchAttribute
	info []gitattributes.MatchAttribute
}

func loadEOLRules(root string) *eolRules {
	r := &eolRules{root: root, fs: osfs.New(root), dirs: map[string][]gitattributes.MatchAttribute{}}
	if gitDir, err := resolveGitDir(root); err == nil {
		if f, err := os.Open(filepath.Join(gitDir, "info", "attributes")); err == nil {
			r.info, _ = gitattributes.ReadAttributes(f, nil, true)
			f.Close()
		}
	}
	return r
}

func (r *eolRules) dir(d string) []gitattributes.MatchAttribute {
	if ps, ok := r.dirs[d]; ok {
		return ps
	}
	var domain []string
	if d != "" {
		domain = strings.Split(d, "/")
	}
	ps, _ := gitattributes.ReadAttributesFile(r.fs, domain, ".gitattributes", d == "")
	r.dirs[d] = ps
	return ps
}

// applies reports whether a text/eol/crlf rule is specified for path.
func (r *eolRules) applies(path string) bool {
	if r == nil {
		return false
	}
	var chain []string
	for d := pathDir(path); ; d = pathDir(d) {
		chain = append([]string{d}, chain...)
		if d == "" {
			break
		}
	}
	var stack []gitattributes.MatchAttribute
	for _, d := range chain { // root first: deeper files take precedence
		stack = append(stack, r.dir(d)...)
	}
	stack = append(stack, r.info...)
	if len(stack) == 0 {
		return false
	}
	res, _ := gitattributes.NewMatcher(stack).Match(strings.Split(path, "/"), []string{"text", "eol", "crlf"})
	for _, at := range res {
		if !at.IsUnspecified() {
			return true
		}
	}
	return false
}

// dropConfigClean clears the worktree-modified mark of files host git
// calls clean: equal to their index blob once converted per
// core.autocrlf, with an executable-bit difference ignored under
// core.filemode=false.
func dropConfigClean(repo *gogit.Repository, root string, st gogit.Status, autocrlf, fileMode bool) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return
	}
	entries := map[string]plumbing.Hash{}
	trustModes := map[string]bool{} // true: the executable bit cannot be why it differs
	for _, e := range idx.Entries {
		if e.Mode == filemode.Regular || e.Mode == filemode.Executable {
			entries[e.Name] = e.Hash
			trustModes[e.Name] = !fileMode || diskModeMatches(filepath.Join(root, filepath.FromSlash(e.Name)), e.Mode)
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
		if autocrlf {
			if conv, changed := toGit(content, blobHasCR(repo, h)); changed {
				content = conv
			}
		}
		if plumbing.ComputeHash(plumbing.BlobObject, content) != h {
			continue
		}
		if !trustModes[path] {
			continue // same bytes, different executable bit, and filemode is on
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

// coreFileMode resolves core.filemode (default true). Git for Windows'
// init writes core.filemode=false: the executable bit is then never read
// from disk — an existing index entry keeps its mode, a new file is 100644.
func coreFileMode(repo *gogit.Repository) bool {
	v := ""
	if cfg, err := repo.Config(); err == nil {
		v = cfg.Raw.Section("core").Option("filemode")
	}
	if v == "" {
		for _, p := range gitConfigFiles() {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			c := config.NewConfig()
			if c.Unmarshal(data) == nil {
				if v = c.Raw.Section("core").Option("filemode"); v != "" {
					break
				}
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "no", "off", "0":
		return false
	}
	return true
}

// finishAdd brings what a go-git add just staged to what host git's add
// stores: core.autocrlf conversion, and under core.filemode=false the
// index's existing mode (100644 for a new file) instead of the disk's.
func finishAdd(repo *gogit.Repository, root string, before map[string]plumbing.Hash, beforeMode map[string]filemode.FileMode) error {
	if err := applyAutoCRLFToAdded(repo, root, before); err != nil {
		return err
	}
	if coreFileMode(repo) {
		return nil
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	changed := false
	for _, e := range idx.Entries {
		if e.Mode != filemode.Regular && e.Mode != filemode.Executable {
			continue
		}
		want := filemode.Regular
		if m, ok := beforeMode[e.Name]; ok && (m == filemode.Regular || m == filemode.Executable) {
			want = m
		}
		if e.Mode != want {
			e.Mode = want
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return repo.Storer.SetIndex(idx)
}

// indexModes records each index path's mode before an add.
func indexModes(repo *gogit.Repository) map[string]filemode.FileMode {
	out := map[string]filemode.FileMode{}
	if idx, err := repo.Storer.Index(); err == nil {
		for _, e := range idx.Entries {
			out[e.Name] = e.Mode
		}
	}
	return out
}

func diskModeMatches(path string, m filemode.FileMode) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return (fi.Mode()&0o111 != 0) == (m == filemode.Executable)
}
