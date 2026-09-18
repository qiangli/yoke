// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package scope is the one git-repo-aware store resolver shared by every
// stateful bashy verb whose data has both a per-repo and a per-host home
// (today: todo and kb; the same shape fits any future "replaces an ad-hoc
// markdown file" store).
//
// The rule is uniform so an agent learns it once:
//
//	default            inside a git repo → THAT repo's committed <RepoSub>/
//	                   (travels with the clone, shows in diffs); otherwise the
//	                   per-host store (~/.bashy/<tool>/, not committed).
//	--base-dir <root>  another project root's committed store (<root>/<RepoSub>/)
//	                   — so one agent can travel repos in a single session.
//	--user             force the per-host store even inside a repo.
//	--repo             force the repo store (error if not inside a git repo).
//
// scope resolves only the DIRECTORY + a human label; each tool builds its own
// store type on top (todo -> issue.Store, kb -> kb.Store), so the store shapes
// stay independent while the "where does it live" logic is one code path.
package scope

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Kind is the resolved scope.
type Kind string

const (
	KindRepo  Kind = "repo"  // a git repo's committed store
	KindUser  Kind = "user"  // the per-host store
	KindAgent Kind = "agent" // an owner-only store under the per-identity agent-data dir
)

// Scope is a resolved location plus the pieces a tool needs to build its store.
type Scope struct {
	Kind  Kind
	Root  string // base directory (repo root, or the per-host base dir)
	Sub   string // subdirectory under Root ("" is allowed)
	Owner string // per-host owner segment (user scope only; may be "")
}

// Dir is the on-disk directory the store lives in.
func (s *Scope) Dir() string { return filepath.Join(s.Root, s.Sub) }

// Label is the short "which store am I on" line ("repo <root>" | "agent
// <root>" | "user <owner>").
func (s *Scope) Label() string {
	if s.Kind == KindRepo {
		return "repo " + s.Root
	}
	if s.Kind == KindAgent {
		return "agent " + s.Root
	}
	if s.Owner != "" {
		return "user " + s.Owner
	}
	return "user " + s.Root
}

// Options drive Resolve. RepoSub is the committed subdir (e.g. "docs/todo");
// HostDir is called ONLY when the per-host store is chosen, so a tool that is
// always used inside a repo never pays the home lookup (and never fails on it).
type Options struct {
	RepoSub    string                 // committed subdir under the repo root, e.g. "docs/kb"
	Owner      string                 // per-host owner segment ("" = no owner subdir)
	HostDir    func() (string, error) // lazy per-host base directory (e.g. ~/.bashy/kb)
	AgentDir   func() (string, error) // lazy agent-ring directory (the full <agent-data>/kb); called only for ForceAgent
	ForceRepo  bool                   // --repo
	ForceUser  bool                   // --user
	ForceAgent bool                   // --ring agent: the owner-only per-identity store (never a default)
	BaseDir    string                 // --base-dir: an explicit repo root
}

// Resolve applies the precedence documented on the package: base-dir > (unless
// --user) auto-detected git repo > --repo error > per-host store. The agent
// ring is never a default — it is returned only when ForceAgent is set (an
// explicit --ring agent), so a bare Resolve keeps the repo/host behavior
// unchanged.
func Resolve(o Options) (*Scope, error) {
	if o.ForceAgent {
		if o.AgentDir == nil {
			return nil, fmt.Errorf("agent ring: no agent-data directory available")
		}
		dir, err := o.AgentDir()
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("agent ring: no per-agent store (this process was not launched with a per-identity agent-data dir)")
		}
		// AgentDir returns the full ring directory; no owner subdir applies.
		return &Scope{Kind: KindAgent, Root: dir, Sub: ""}, nil
	}
	if b := strings.TrimSpace(o.BaseDir); b != "" {
		return &Scope{Kind: KindRepo, Root: b, Sub: o.RepoSub}, nil
	}
	if !o.ForceUser {
		if root, ok := FindGitRoot(); ok {
			return &Scope{Kind: KindRepo, Root: root, Sub: o.RepoSub}, nil
		}
		if o.ForceRepo {
			return nil, fmt.Errorf("--repo: not inside a git repo (a .git was not found here or in any parent)")
		}
	}
	if o.HostDir == nil {
		return nil, fmt.Errorf("scope: no per-host directory available")
	}
	host, err := o.HostDir()
	if err != nil {
		return nil, err
	}
	owner := SanitizeSegment(o.Owner)
	return &Scope{Kind: KindUser, Root: host, Sub: owner, Owner: owner}, nil
}

// FindGitRoot walks up from the current directory for a `.git` entry (a
// directory, or a file for worktrees/submodules). Returns the repo root and
// true, or "" and false when the cwd is not inside a git repo.
func FindGitRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// SanitizeSegment reduces a caller-supplied name to one safe path segment (no
// traversal, no separators). An empty input stays empty — callers that need a
// default supply their own.
func SanitizeSegment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	return s
}
