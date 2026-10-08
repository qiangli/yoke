package gomod

import (
	"path/filepath"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	coregit "github.com/qiangli/yoke/git"
)

// State of one sibling pin against the sibling's checked-out HEAD.
type State string

const (
	InSync  State = "in-sync"
	Stale   State = "stale"
	Unknown State = "unknown" // cannot be compared: placeholder, local replace, missing tag or repo
)

// Drift is one pin of Module on Sibling.
type Drift struct {
	Module  string `json:"module"`
	Sibling string `json:"sibling"` // sibling module path
	Name    string `json:"name"`    // sibling dir relative to the workspace root
	Version string `json:"version"` // pinned version ("" for a local replace)
	Head    string `json:"head,omitempty"`
	State   State  `json:"state"`
	Reason  string `json:"reason,omitempty"`
}

// ResolveFunc resolves rev (HEAD or a tag) in the repo at dir to a full SHA.
type ResolveFunc func(dir, rev string) (string, error)

// DefaultResolve reads git through yoke's pure-Go git.
var DefaultResolve ResolveFunc = coregit.ResolveCommit

// Drift reports every sibling pin of m. A pin is in sync when its
// pseudo-version revision prefixes the sibling HEAD, or its tag peels to
// HEAD. A nested module's tag carries its subdirectory prefix (pkg/oci/v1.2.3).
func (ws *Workspace) Drift(m *Module, resolve ResolveFunc) []Drift {
	if resolve == nil {
		resolve = DefaultResolve
	}
	var out []Drift
	for _, r := range ws.SiblingRequires(m) {
		d := Drift{Module: m.Path, Sibling: r.Path, Name: ws.Name(r.Sibling.Dir), Version: r.Version}
		repo := ws.RepoDir(r.Sibling.Dir)
		head, err := resolve(repo, "HEAD")
		if err != nil {
			d.State, d.Reason = Unknown, "sibling HEAD: "+err.Error()
			out = append(out, d)
			continue
		}
		d.Head = head
		d.State, d.Reason = compare(r, head, func(tag string) (string, error) {
			if sub := strings.TrimPrefix(ws.Name(r.Sibling.Dir), ws.Name(repo)); sub != "" {
				tag = strings.TrimPrefix(sub, "/") + "/" + tag
			}
			return resolve(repo, "refs/tags/"+tag)
		})
		out = append(out, d)
	}
	return out
}

func compare(r Require, head string, tagCommit func(string) (string, error)) (State, string) {
	v := r.Version
	switch {
	case r.Local:
		return Unknown, "local path replace " + r.Via
	case module.IsPseudoVersion(v):
		rev, err := module.PseudoVersionRev(v)
		if err != nil || strings.Trim(rev, "0") == "" {
			return Unknown, "placeholder pseudo-version"
		}
		if strings.HasPrefix(strings.ToLower(head), strings.ToLower(rev)) {
			return InSync, ""
		}
		return Stale, "pinned " + rev + ", HEAD " + short(head)
	case !semver.IsValid(v) || v == "v0.0.0":
		return Unknown, "placeholder version " + v
	}
	c, err := tagCommit(v)
	if err != nil {
		return Unknown, "tag " + v + " not found"
	}
	if strings.EqualFold(c, head) {
		return InSync, ""
	}
	return Stale, "tag " + v + " is " + short(c) + ", HEAD " + short(head)
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// StaleSiblings names the siblings of the module at dir whose pins are stale
// or unknown. No workspace, or dir not a workspace module, means nothing to
// check and returns nil.
func StaleSiblings(dir string) []string {
	ws, err := Load(dir)
	if err != nil || ws == nil {
		return nil
	}
	m := ws.Module(dir)
	if m == nil {
		return nil
	}
	var names []string
	for _, d := range ws.Drift(m, nil) {
		if d.State != InSync {
			names = append(names, filepath.ToSlash(d.Name))
		}
	}
	return names
}
