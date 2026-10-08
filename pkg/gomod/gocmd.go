package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Runner runs `go args...` in dir with env appended to the environment and
// returns stdout. Tests replace it; nothing here reimplements the go command.
type Runner func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)

// RunGo is the default Runner: the go on PATH (bashy's own `bashy go`
// provisions one when there is none).
var RunGo Runner = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// pinnedEnv makes go build exactly what go.mod declares: no workspace, and
// go.mod may be updated by the command that was asked to update it.
var pinnedEnv = []string{"GOWORK=off", "GOFLAGS=-mod=mod"}

// Change is one pin Sync moved.
type Change struct {
	Module  string `json:"module"`
	Sibling string `json:"sibling"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// Sync moves every stale or unknown pin of m to its sibling's HEAD with the
// go command — `go get S@HEAD`, or for a versioned fork replace
// `go mod edit -replace` — then runs `go mod tidy` once. A local path replace
// is left alone: dropping it is a migration decision, not a sync. only, when
// non-empty, limits the siblings (by module path or name).
func (ws *Workspace) Sync(ctx context.Context, m *Module, only []string, resolve ResolveFunc, run Runner) ([]Change, error) {
	if run == nil {
		run = RunGo
	}
	want := map[string]bool{}
	for _, o := range only {
		want[o] = true
	}
	reqs := map[string]Require{}
	for _, r := range ws.SiblingRequires(m) {
		reqs[r.Path] = r
	}
	var changes []Change
	for _, d := range ws.Drift(m, resolve) {
		r := reqs[d.Sibling]
		if d.State == InSync || d.Head == "" || r.Local || (len(want) > 0 && !want[d.Sibling] && !want[d.Name]) {
			continue
		}
		args := []string{"get", d.Sibling + "@" + d.Head}
		if r.Via != "" {
			args = []string{"mod", "edit", "-replace=" + d.Sibling + "=" + r.Via + "@" + d.Head}
		}
		if _, err := run(ctx, m.Dir, pinnedEnv, args...); err != nil {
			return changes, err
		}
		changes = append(changes, Change{Module: m.Path, Sibling: d.Sibling, From: d.Version})
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if _, err := run(ctx, m.Dir, pinnedEnv, "mod", "tidy"); err != nil {
		return changes, err
	}
	if fresh, err := ListWorkspace(ws.WorkFile); err == nil {
		if fm := fresh.Module(m.Dir); fm != nil {
			now := map[string]string{}
			for _, r := range fresh.SiblingRequires(fm) {
				now[r.Path] = r.Version
			}
			for i := range changes {
				changes[i].To = now[changes[i].Sibling]
			}
		}
	}
	return changes, nil
}

// Info is `go mod download -json` for one module version.
type Info struct {
	Path     string  `json:"Path"`
	Version  string  `json:"Version"`
	Error    string  `json:"Error,omitempty"`
	Dir      string  `json:"Dir,omitempty"`
	Zip      string  `json:"Zip,omitempty"`
	Sum      string  `json:"Sum,omitempty"`
	GoModSum string  `json:"GoModSum,omitempty"`
	Origin   *Origin `json:"Origin,omitempty"`
}

// Origin is where the go command fetched a module version from.
type Origin struct {
	VCS    string `json:"VCS,omitempty"`
	URL    string `json:"URL,omitempty"`
	Subdir string `json:"Subdir,omitempty"`
	Hash   string `json:"Hash,omitempty"`
	Ref    string `json:"Ref,omitempty"`
}

// ModuleDir downloads path@version into the module cache (verified against
// go.sum / the checksum database by the go command) and returns its info;
// Info.Dir holds the extracted content, for Go and non-Go modules alike.
// The version must be pinned: a semver tag, a pseudo-version or a commit
// hash, never a moving query like latest or a branch. offline sets
// GOPROXY=off so only the cache answers.
func ModuleDir(ctx context.Context, spec string, offline bool, run Runner) (Info, error) {
	if run == nil {
		run = RunGo
	}
	path, version, ok := strings.Cut(spec, "@")
	if !ok || version == "" {
		return Info{}, &Error{Code: weavecli.ExitInvalidArg, Msg: spec + ": want path@version"}
	}
	if err := module.CheckPath(path); err != nil {
		return Info{}, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error()}
	}
	if !semver.IsValid(version) && !isHex(version) {
		return Info{}, &Error{Code: weavecli.ExitInvalidArg, Msg: spec + ": version must be pinned (tag, pseudo-version or commit hash)"}
	}
	tmp, err := os.MkdirTemp("", "gomod-dir-")
	if err != nil {
		return Info{}, err
	}
	defer os.RemoveAll(tmp)
	env := pinnedEnv
	if offline {
		env = append(append([]string{}, env...), "GOPROXY=off")
	}
	out, runErr := run(ctx, tmp, env, "mod", "download", "-json", spec)
	var info Info
	if err := json.Unmarshal(out, &info); err != nil {
		if runErr != nil {
			return Info{}, runErr
		}
		return Info{}, fmt.Errorf("go mod download -json: %w", err)
	}
	if info.Error != "" {
		return info, &Error{Code: weavecli.ExitDepUnhealthy, Msg: info.Error}
	}
	if runErr != nil {
		return info, runErr
	}
	return info, nil
}

func isHex(s string) bool {
	if len(s) < 12 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// Tool is a `tool` directive and the module version that provides it.
type Tool struct {
	Package string `json:"package"`
	Module  string `json:"module"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"` // pseudo-version revision, when there is one
}

// Tools lists the tool directives of a go.mod and the pinned version of the
// module providing each (the longest required module path prefixing the
// package; a versioned replace wins). This is how a release names the exact
// build-only sibling it ships: read go.mod at the tag.
func Tools(gomod []byte) ([]Tool, error) {
	f, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return nil, err
	}
	var out []Tool
	for _, t := range f.Tool {
		tl := Tool{Package: t.Path}
		for _, r := range f.Require {
			p := r.Mod.Path
			if (t.Path == p || strings.HasPrefix(t.Path, p+"/")) && len(p) > len(tl.Module) {
				tl.Module, tl.Version = p, r.Mod.Version
			}
		}
		for _, rp := range f.Replace {
			if rp.Old.Path == tl.Module && rp.New.Version != "" {
				tl.Version = rp.New.Version
			}
		}
		if rev, err := module.PseudoVersionRev(tl.Version); err == nil {
			tl.Commit = rev
		}
		out = append(out, tl)
	}
	return out, nil
}

// Error carries a weavecli exit code with the message.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// ExitCode is the stable weavecli exit code.
func (e *Error) ExitCode() int { return e.Code }

// ExitCodeOf maps err to a weavecli exit code (0 for nil, 1 by default).
func ExitCodeOf(err error) int {
	if err == nil {
		return weavecli.ExitOK
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return weavecli.ExitGenericFail
}
