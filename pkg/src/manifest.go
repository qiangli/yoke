package src

import "strings"

// ProtEntry is one protected-manifest row: a path prefix inside a source
// repo. Core rows are the trust root (class derivation sources, the git and
// provisioning machinery, the binary entry); Protected rows are agent
// orchestration and audit surface that skills must not rewrite.
type ProtEntry struct {
	// Repo is the source repo name (yoke, bashy, sh, ...).
	Repo string
	// Path is a repo-relative file or directory prefix (slash-separated).
	Path string
	// Core is true for trust-root paths, false for protected (non-core) ones.
	Core bool
}

// Kind returns "core" or "protected" for reports.
func (e ProtEntry) Kind() string {
	if e.Core {
		return "core"
	}
	return "protected"
}

// ProtectedManifest transcribes Appendix A of the Sprint 458 M0 record: the
// yoke paths plus the bashy/sh paths as repo-qualified strings. `src check`
// reports any working-tree edit under one of these prefixes as a violation,
// and ClassOf reports a non-core, non-external command implemented under a
// Protected prefix as ClassProtected.
func ProtectedManifest() []ProtEntry {
	return []ProtEntry{
		// Yoke trust root (core).
		{Repo: "yoke", Path: "go.mod", Core: true},
		{Repo: "yoke", Path: "go.sum", Core: true},
		{Repo: "yoke", Path: "git", Core: true},
		{Repo: "yoke", Path: "pkg/atlas", Core: true},
		{Repo: "yoke", Path: "pkg/binmgr", Core: true},
		{Repo: "yoke", Path: "pkg/src", Core: true},
		{Repo: "yoke", Path: "cmds/all", Core: true},
		{Repo: "yoke", Path: "cmd/yoke", Core: true},
		// Agent orchestration and audit surface (protected, non-core).
		{Repo: "yoke", Path: "pkg/weave", Core: false},
		{Repo: "yoke", Path: "pkg/steward", Core: false},
		{Repo: "yoke", Path: "pkg/policy", Core: false},
		// Shell merge points (repo-qualified; scanned when fetched).
		{Repo: "bashy", Path: "internal/agentos", Core: false},
		{Repo: "sh", Path: "syntax", Core: false},
		{Repo: "sh", Path: "expand", Core: false},
		{Repo: "sh", Path: "interp", Core: false},
	}
}

// matchProt reports the manifest entry covering repo-relative path p in repo,
// or nil. A row covers p when p equals the row's Path or lies beneath it.
func matchProt(repo, p string) *ProtEntry {
	p = strings.TrimPrefix(p, "./")
	for _, e := range ProtectedManifest() {
		if e.Repo != repo {
			continue
		}
		if p == e.Path || strings.HasPrefix(p, e.Path+"/") {
			dup := e
			return &dup
		}
	}
	return nil
}

// manifestRepos returns the repos named by the manifest, for check's scan.
func manifestRepos() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range ProtectedManifest() {
		if !seen[e.Repo] {
			seen[e.Repo] = true
			out = append(out, e.Repo)
		}
	}
	return out
}
