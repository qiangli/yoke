// Package gomod is the Bash# project contract: every project is a Go module,
// Go or not, and the go command is the only version policy engine. A
// multi-repo workspace is a go.work; a sibling pin is a go.mod require (or a
// versioned fork replace). There is no other manifest.
//
// This package only reads go.work/go.mod (golang.org/x/mod/modfile) and shells
// out to go for anything that resolves a version. See
// docs/go-module-contract-plan.md in the dhnt umbrella.
package gomod

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// Module is one workspace member: its module path, directory and parsed go.mod.
type Module struct {
	Path string
	Dir  string
	File *modfile.File
}

// Workspace is a parsed go.work and the go.mod of every module it uses.
type Workspace struct {
	Root     string // directory holding go.work
	WorkFile string
	Modules  []*Module
	byPath   map[string]*Module
	byDir    map[string]*Module
}

// Require is a module's pin on another workspace module. Version is the
// effective pinned version: a versioned replace (a fork) wins over the
// require line. Via is the replacement module path, or the local path when
// the replace points at a directory (then Version is empty).
type Require struct {
	Path    string
	Version string
	Via     string
	Local   bool
	Sibling *Module
}

// FindWorkspace returns the go.work governing start, following the go
// command: GOWORK=off means none, an explicit GOWORK path wins, otherwise
// walk up from start.
func FindWorkspace(start string) (string, bool) {
	switch gw := os.Getenv("GOWORK"); {
	case gw == "off":
		return "", false
	case gw != "" && gw != "auto":
		if _, err := os.Stat(gw); err == nil {
			return gw, true
		}
		return "", false
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, "go.work")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Load finds and parses the workspace governing start. It returns (nil, nil)
// when there is none: a standalone clone has no workspace, and that is not an
// error.
func Load(start string) (*Workspace, error) {
	wf, ok := FindWorkspace(start)
	if !ok {
		return nil, nil
	}
	return ListWorkspace(wf)
}

// ListWorkspace parses workFile and the go.mod of every `use` directory.
func ListWorkspace(workFile string) (*Workspace, error) {
	data, err := os.ReadFile(workFile)
	if err != nil {
		return nil, err
	}
	wf, err := modfile.ParseWork(workFile, data, nil)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(workFile)
	ws := &Workspace{Root: root, WorkFile: workFile, byPath: map[string]*Module{}, byDir: map[string]*Module{}}
	for _, u := range wf.Use {
		dir := filepath.FromSlash(u.Path)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		dir = filepath.Clean(dir)
		gm := filepath.Join(dir, "go.mod")
		b, err := os.ReadFile(gm)
		if err != nil {
			return nil, fmt.Errorf("go.work uses %s: %w", u.Path, err)
		}
		f, err := modfile.Parse(gm, b, nil)
		if err != nil {
			return nil, err
		}
		if f.Module == nil {
			return nil, fmt.Errorf("%s: no module directive", gm)
		}
		m := &Module{Path: f.Module.Mod.Path, Dir: dir, File: f}
		ws.Modules = append(ws.Modules, m)
		ws.byPath[m.Path] = m
		ws.byDir[dir] = m
	}
	return ws, nil
}

// Module returns the workspace module rooted at dir, or nil.
func (ws *Workspace) Module(dir string) *Module {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	return ws.byDir[filepath.Clean(abs)]
}

// ByPath returns the workspace module with module path p, or nil.
func (ws *Workspace) ByPath(p string) *Module { return ws.byPath[p] }

// SiblingRequires lists m's pins on other workspace modules, sorted by path.
func (ws *Workspace) SiblingRequires(m *Module) []Require {
	var out []Require
	for _, r := range m.File.Require {
		sib := ws.byPath[r.Mod.Path]
		if sib == nil || sib == m {
			continue
		}
		req := Require{Path: r.Mod.Path, Version: r.Mod.Version, Sibling: sib}
		for _, rp := range m.File.Replace {
			if rp.Old.Path != r.Mod.Path || (rp.Old.Version != "" && rp.Old.Version != r.Mod.Version) {
				continue
			}
			req.Via = rp.New.Path
			req.Local = rp.New.Version == ""
			req.Version = rp.New.Version
		}
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// RepoDir is the git repository holding the module at dir: the nearest
// directory at or above dir with a .git entry (a directory, or a submodule's
// gitdir file), below the workspace root — the umbrella itself is never a
// sibling. Without one, it is the first path element under the root
// (yoke/pkg/oci -> yoke); a module outside the root is its own repo.
func (ws *Workspace) RepoDir(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if d == ws.Root || d == filepath.Dir(d) {
			break
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
	}
	rel, err := filepath.Rel(ws.Root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return dir
	}
	return filepath.Join(ws.Root, strings.Split(filepath.ToSlash(rel), "/")[0])
}

// Name is the slash-separated path of dir relative to the workspace root, the
// name a sibling is reported under.
func (ws *Workspace) Name(dir string) string {
	rel, err := filepath.Rel(ws.Root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return dir
	}
	return filepath.ToSlash(rel)
}

// SiblingDirs is the transitive set of top-level sibling repo directories m
// depends on through workspace pins, excluding m's own repo, sorted.
// Nested modules collapse to their repo: cloning the repo satisfies them all.
func (ws *Workspace) SiblingDirs(m *Module) []string {
	own := ws.RepoDir(m.Dir)
	seen := map[*Module]bool{m: true}
	repos := map[string]bool{}
	queue := []*Module{m}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range ws.SiblingRequires(cur) {
			if seen[r.Sibling] {
				continue
			}
			seen[r.Sibling] = true
			queue = append(queue, r.Sibling)
			if rd := ws.RepoDir(r.Sibling.Dir); rd != own {
				repos[rd] = true
			}
		}
	}
	out := make([]string, 0, len(repos))
	for d := range repos {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
