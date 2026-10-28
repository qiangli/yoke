package src

import "strings"

// ProtEntry is one protected-manifest row: a path inside a source repo. It
// transcribes Appendix A of the Sprint 458 M0 record exactly: core rows are
// the certified engine (a running core never executes a local edit) and
// protected rows are guard rails (agent orchestration and audit surface that
// skills must not rewrite). Everything else in a repo is skill tier and
// editable.
type ProtEntry struct {
	// Repo is the source repo name (yoke, bashy, sh, coreutils, readline).
	Repo string
	// Path is a repo-relative file or directory. "" means the whole repo.
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

// ProtectedManifest transcribes Appendix A of the Sprint 458 M0 record. Core
// rows come first so matchProt resolves a path covered by both tiers (the sh
// bashpp decorators) as core.
func ProtectedManifest() []ProtEntry {
	return []ProtEntry{
		// Core: the certified engine.
		{Repo: "sh", Path: "syntax", Core: true},
		{Repo: "sh", Path: "expand", Core: true},
		{Repo: "sh", Path: "interp", Core: true},
		{Repo: "sh", Path: "pattern", Core: true},
		{Repo: "sh", Path: "fileutil", Core: true},
		{Repo: "sh", Path: "shell", Core: true},
		{Repo: "sh", Path: "winmode", Core: true},
		{Repo: "sh", Path: "pathconv", Core: true},
		{Repo: "sh", Path: "lower", Core: true},
		{Repo: "sh", Path: "polyglot", Core: true},
		{Repo: "coreutils", Path: "", Core: true},
		{Repo: "readline", Path: "", Core: true},
		{Repo: "yoke", Path: "pkg/atlas", Core: true},
		{Repo: "yoke", Path: "pkg/src", Core: true},
		{Repo: "yoke", Path: "go.mod", Core: true},
		{Repo: "yoke", Path: "go.sum", Core: true},
		// Protected: guard rails (already core-covered sh rows kept anyway).
		{Repo: "sh", Path: "syntax/bashpp_decorator.go", Core: false},
		{Repo: "sh", Path: "interp/bashpp_decorator.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/decorators.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/decorators_std.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/confirm.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/contain.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/contain_other.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/contain_egress.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/contain_container.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/contain_image.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/limit.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/contracts.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/attest.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/audit.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/inspect_decorators.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/coord.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/verify.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/self.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/self_image.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/self_pair.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/self_seed.go", Core: false},
		{Repo: "bashy", Path: "internal/agentos/resource_admission.go", Core: false},
		{Repo: "yoke", Path: "pkg/policy", Core: false},
		{Repo: "yoke", Path: "pkg/procguard", Core: false},
		{Repo: "yoke", Path: "pkg/secrets", Core: false},
		{Repo: "yoke", Path: "pkg/redact", Core: false},
		{Repo: "yoke", Path: "pkg/ask", Core: false},
		{Repo: "yoke", Path: "pkg/binmgr", Core: false},
		{Repo: "yoke", Path: "pkg/release", Core: false},
		{Repo: "yoke", Path: "pkg/admission", Core: false},
		{Repo: "yoke", Path: "pkg/mplfree", Core: false},
		{Repo: "yoke", Path: "pkg/resources/registry.go", Core: false},
		{Repo: "yoke", Path: "pkg/resources/limits.go", Core: false},
		{Repo: "yoke", Path: "pkg/resources/budget.go", Core: false},
		{Repo: "yoke", Path: "pkg/resources/disk_floor.go", Core: false},
		{Repo: "yoke", Path: "pkg/weave/sprint_commit_attribution.go", Core: false},
		{Repo: "yoke", Path: "pkg/weave/weave_story_commit.go", Core: false},
		// The browser sensitive-input guard (Sprint 512.5): the gate, the
		// shared validators that decide what it blocks, its human-path
		// adapter, its page-side field check, and each backend's wiring. The
		// extension's background.js (the guard tab pin) and page.js (the
		// in-page fill verify) carry guard logic too.
		{Repo: "yoke", Path: "pkg/browser/guard", Core: false},
		{Repo: "yoke", Path: "pkg/sensitive", Core: false},
		{Repo: "yoke", Path: "pkg/browser/guardask", Core: false},
		{Repo: "yoke", Path: "pkg/browser/live/guard.go", Core: false},
		{Repo: "yoke", Path: "pkg/browser/live/extension/guard.js", Core: false},
		{Repo: "yoke", Path: "pkg/browser/live/extension/background.js", Core: false},
		{Repo: "yoke", Path: "pkg/browser/live/extension/page.js", Core: false},
		{Repo: "yoke", Path: "pkg/browser/internal/cdpactions/guard.go", Core: false},
	}
}

// matchProt reports the manifest entry covering repo-relative path p in repo,
// or nil. A row covers p when the row names the whole repo (Path ""), when p
// equals the row's Path, or when p lies beneath it. Core rows are listed
// first and win on overlap.
func matchProt(repo, p string) *ProtEntry {
	p = strings.TrimPrefix(p, "./")
	for _, e := range ProtectedManifest() {
		if e.Repo != repo {
			continue
		}
		if e.Path == "" || p == e.Path || strings.HasPrefix(p, e.Path+"/") {
			dup := e
			return &dup
		}
	}
	return nil
}

// PathClass returns the path tier of repo-relative path p in repo: core or
// protected when a manifest row covers it, skill otherwise. Protection is per
// path, not per command — commands classify as core, skill, or external and
// never inherit a path tier.
func PathClass(repo, p string) Class {
	if m := matchProt(repo, p); m != nil {
		if m.Core {
			return ClassCore
		}
		return ClassProtected
	}
	return ClassSkill
}

// PackageProtectedParts returns the protected (non-core) manifest rows in the
// repo-relative package directory dir: rows equal to dir or beneath it. It
// backs `src show`'s protected_parts list — the guard-rail files inside a
// command's own package.
func PackageProtectedParts(repo, dir string) []ProtEntry {
	dir = strings.Trim(strings.TrimPrefix(dir, "./"), "/")
	var out []ProtEntry
	for _, e := range ProtectedManifest() {
		if e.Repo != repo || e.Core {
			continue
		}
		if dir == "" || e.Path == dir || strings.HasPrefix(e.Path, dir+"/") {
			out = append(out, e)
		}
	}
	return out
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
