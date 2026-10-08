package gomod

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/qiangli/coreutils/pkg/weavecli"
	coregit "github.com/qiangli/yoke/git"
)

// InitResult reports what Init wrote.
type InitResult struct {
	Module    string   `json:"module"`
	Created   []string `json:"created,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Init makes the project at dir a Go module: go.mod (module path = VCS path)
// and, when the directory has no Go source, a doc-only pkg.go so go
// build/vet/get treat it as a package. modPath empty derives the path from the
// origin remote and fails closed without one. An existing go.mod is never
// rewritten; a different module path in it is an error. Pure x/mod: no go
// command is run.
func Init(dir, modPath, goVersion string) (InitResult, error) {
	var res InitResult
	gm := filepath.Join(dir, "go.mod")
	existing, readErr := os.ReadFile(gm)
	if modPath == "" && readErr == nil {
		if f, err := modfile.ParseLax(gm, existing, nil); err == nil && f.Module != nil {
			modPath = f.Module.Mod.Path
		}
	}
	if modPath == "" {
		p, err := originModulePath(dir)
		if err != nil {
			return res, &Error{Code: weavecli.ExitInputRequired, Msg: err.Error() + "; pass --module PATH"}
		}
		modPath = p
	}
	if err := module.CheckPath(modPath); err != nil {
		return res, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error()}
	}
	res.Module = modPath

	if readErr == nil {
		f, err := modfile.ParseLax(gm, existing, nil)
		if err != nil {
			return res, err
		}
		if f.Module == nil || f.Module.Mod.Path != modPath {
			return res, &Error{Code: weavecli.ExitStateConflict, Msg: fmt.Sprintf("%s declares a different module; not rewriting", gm)}
		}
		res.Unchanged = append(res.Unchanged, "go.mod")
	} else {
		if goVersion == "" {
			goVersion = defaultGoVersion()
		}
		f := &modfile.File{}
		if err := f.AddModuleStmt(modPath); err != nil {
			return res, err
		}
		if err := f.AddGoStmt(goVersion); err != nil {
			return res, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error()}
		}
		out, err := f.Format()
		if err != nil {
			return res, err
		}
		if err := os.WriteFile(gm, out, 0o644); err != nil {
			return res, err
		}
		res.Created = append(res.Created, "go.mod")
	}

	gos, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	switch {
	case len(gos) > 0 && !slices.Contains(gos, filepath.Join(dir, "pkg.go")):
		// A Go project needs no stub.
	case len(gos) > 0:
		res.Unchanged = append(res.Unchanged, "pkg.go")
	default:
		if err := os.WriteFile(filepath.Join(dir, "pkg.go"), []byte(pkgGo(modPath)), 0o644); err != nil {
			return res, err
		}
		res.Created = append(res.Created, "pkg.go")
	}

	if lic, _ := filepath.Glob(filepath.Join(dir, "LICENSE*")); len(lic) == 0 {
		if cp, _ := filepath.Glob(filepath.Join(dir, "COPYING*")); len(cp) == 0 {
			res.Warnings = append(res.Warnings, "no LICENSE file")
		}
	}
	return res, nil
}

func pkgGo(modPath string) string {
	return fmt.Sprintf(`// Package %s is the Go module face of %s.
//
// This module carries no Go code. It exists so the Go module system
// (go get, go mod download, go.sum, GOPROXY) pins, fetches and verifies
// this project's content.
package %s
`, PackageName(modPath), modPath, PackageName(modPath))
}

// PackageName derives a Go package identifier from a module path: the last
// element (a /vN major suffix skipped), lowercased, letters and digits only.
func PackageName(modPath string) string {
	elems := strings.Split(modPath, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = elems[len(elems)-2]
	}
	var b strings.Builder
	for _, c := range strings.ToLower(last) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	name := b.String()
	if name == "" || name[0] <= '9' {
		name = "x" + name
	}
	return name
}

func defaultGoVersion() string {
	v := strings.TrimPrefix(runtime.Version(), "go")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 && parts[0] != "" && strings.Trim(parts[1], "0123456789") == "" {
		return parts[0] + "." + parts[1]
	}
	return "1.24"
}

func originModulePath(dir string) (string, error) {
	_, remotes, err := coregit.Remotes(dir)
	if err != nil {
		return "", err
	}
	for _, r := range remotes {
		if r.Name == "origin" && len(r.URLs) > 0 {
			return ModulePathFromRemote(r.URLs[0])
		}
	}
	return "", fmt.Errorf("no origin remote")
}

// ModulePathFromRemote turns a git remote URL into a module path:
// git@host:owner/repo.git and https://host/owner/repo(.git) become
// host/owner/repo.
func ModulePathFromRemote(u string) (string, error) {
	s := strings.TrimSpace(u)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if at, colon := strings.Index(s, "@"), strings.Index(s, ":"); at >= 0 && colon > at {
		s = s[:colon] + "/" + s[colon+1:] // scp-like git@host:owner/repo
	}
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	if err := module.CheckPath(s); err != nil {
		return "", fmt.Errorf("remote %s: %w", u, err)
	}
	return s, nil
}
