package src_test

import (
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/src"
)

// TestManifestTranscribesAppendixA pins the protected manifest row-for-row
// against the Sprint 458 M0 record: the certified-engine core paths, then
// the guard-rail protected paths. Skill-tier directories (bashy's
// internal/agentos as a whole, yoke's pkg/weave and pkg/steward) must never
// appear whole — only the listed files.
func TestManifestTranscribesAppendixA(t *testing.T) {
	core := func(repo, path string) src.ProtEntry { return src.ProtEntry{Repo: repo, Path: path, Core: true} }
	prot := func(repo, path string) src.ProtEntry { return src.ProtEntry{Repo: repo, Path: path, Core: false} }
	want := []src.ProtEntry{
		core("sh", "syntax"),
		core("sh", "expand"),
		core("sh", "interp"),
		core("sh", "pattern"),
		core("sh", "fileutil"),
		core("sh", "shell"),
		core("sh", "winmode"),
		core("sh", "pathconv"),
		core("sh", "lower"),
		core("sh", "polyglot"),
		core("coreutils", ""),
		core("readline", ""),
		core("yoke", "pkg/atlas"),
		core("yoke", "pkg/src"),
		core("yoke", "go.mod"),
		core("yoke", "go.sum"),
		prot("sh", "syntax/bashpp_decorator.go"),
		prot("sh", "interp/bashpp_decorator.go"),
		prot("bashy", "internal/agentos/decorators.go"),
		prot("bashy", "internal/agentos/decorators_std.go"),
		prot("bashy", "internal/agentos/confirm.go"),
		prot("bashy", "internal/agentos/contain.go"),
		prot("bashy", "internal/agentos/contain_other.go"),
		prot("bashy", "internal/agentos/contain_egress.go"),
		prot("bashy", "internal/agentos/contain_container.go"),
		prot("bashy", "internal/agentos/contain_image.go"),
		prot("bashy", "internal/agentos/limit.go"),
		prot("bashy", "internal/agentos/contracts.go"),
		prot("bashy", "internal/agentos/attest.go"),
		prot("bashy", "internal/agentos/audit.go"),
		prot("bashy", "internal/agentos/inspect_decorators.go"),
		prot("bashy", "internal/agentos/coord.go"),
		prot("bashy", "internal/agentos/verify.go"),
		prot("bashy", "internal/agentos/self.go"),
		prot("bashy", "internal/agentos/self_image.go"),
		prot("bashy", "internal/agentos/self_pair.go"),
		prot("bashy", "internal/agentos/self_seed.go"),
		prot("bashy", "internal/agentos/resource_admission.go"),
		prot("yoke", "pkg/policy"),
		prot("yoke", "pkg/procguard"),
		prot("yoke", "pkg/secrets"),
		prot("yoke", "pkg/redact"),
		prot("yoke", "pkg/ask"),
		prot("yoke", "pkg/binmgr"),
		prot("yoke", "pkg/release"),
		prot("yoke", "pkg/admission"),
		prot("yoke", "pkg/mplfree"),
		prot("yoke", "pkg/resources/registry.go"),
		prot("yoke", "pkg/resources/limits.go"),
		prot("yoke", "pkg/resources/budget.go"),
		prot("yoke", "pkg/resources/disk_floor.go"),
		prot("yoke", "pkg/weave/sprint_commit_attribution.go"),
		prot("yoke", "pkg/weave/weave_story_commit.go"),
		prot("yoke", "pkg/browser/guard"),
		prot("yoke", "pkg/browser/live/guard.go"),
		prot("yoke", "pkg/browser/live/extension/guard.js"),
		prot("yoke", "pkg/browser/internal/cdpactions/guard.go"),
	}
	if got := src.ProtectedManifest(); !slices.Equal(got, want) {
		t.Errorf("ProtectedManifest() =\n%v\nwant\n%v", got, want)
	}
}

// TestPathClassTiers pins per-path protection: Appendix-A files are core or
// protected, skill-tier files stay skill, and whole-repo rows cover
// everything in their repo.
func TestPathClassTiers(t *testing.T) {
	cases := []struct {
		repo, path string
		want       src.Class
		why        string
	}{
		{"yoke", "pkg/atlas/atlas.go", src.ClassCore, "certified engine"},
		{"yoke", "pkg/src/class.go", src.ClassCore, "the class table itself"},
		{"yoke", "go.mod", src.ClassCore, "certified engine root"},
		{"coreutils", "ls.go", src.ClassCore, "whole-repo row"},
		{"coreutils", "src/ls/ls.go", src.ClassCore, "whole-repo row covers depth"},
		{"readline", "anything.go", src.ClassCore, "whole-repo row"},
		{"sh", "interp/interp.go", src.ClassCore, "certified shell runtime"},
		{"sh", "syntax/bashpp_decorator.go", src.ClassCore, "core overlap wins"},
		{"bashy", "internal/agentos/decorators.go", src.ClassProtected, "Appendix-A guard rail"},
		{"bashy", "internal/agentos/resource_admission.go", src.ClassProtected, "Appendix-A guard rail"},
		{"yoke", "pkg/policy/policy.go", src.ClassProtected, "Appendix-A guard rail"},
		{"yoke", "pkg/weave/weave_story_commit.go", src.ClassProtected, "Appendix-A guard rail"},
		{"yoke", "pkg/resources/budget.go", src.ClassProtected, "Appendix-A guard rail"},
		{"bashy", "internal/agentos/sprint.go", src.ClassSkill, "skill tier stays editable"},
		{"yoke", "pkg/weave/weave.go", src.ClassSkill, "skill tier stays editable"},
		{"yoke", "pkg/weave/engine.go", src.ClassSkill, "unlisted file stays editable"},
		{"yoke", "README.md", src.ClassSkill, "unlisted file stays editable"},
		{"yoke", "pkg/steward/steward.go", src.ClassSkill, "dropped wholesale row stays editable"},
	}
	for _, tc := range cases {
		if got := src.PathClass(tc.repo, tc.path); got != tc.want {
			t.Errorf("PathClass(%q, %q) = %q, want %q (%s)", tc.repo, tc.path, got, tc.want, tc.why)
		}
	}
}

// TestPackageProtectedParts pins `src show`'s protected_parts: only the
// protected rows in the command's own package.
func TestPackageProtectedParts(t *testing.T) {
	got := src.PackageProtectedParts("yoke", "pkg/weave")
	var paths []string
	for _, e := range got {
		paths = append(paths, e.Path)
	}
	want := []string{"pkg/weave/sprint_commit_attribution.go", "pkg/weave/weave_story_commit.go"}
	if !slices.Equal(paths, want) {
		t.Errorf("PackageProtectedParts(yoke, pkg/weave) = %q, want %q", paths, want)
	}
	if got := src.PackageProtectedParts("yoke", "pkg/dag"); len(got) != 0 {
		t.Errorf("PackageProtectedParts(yoke, pkg/dag) = %v, want none", got)
	}
	if got := src.PackageProtectedParts("yoke", "pkg/atlas"); len(got) != 0 {
		t.Errorf("PackageProtectedParts(yoke, pkg/atlas) = %v, want none (core rows excluded)", got)
	}
	got = src.PackageProtectedParts("bashy", "internal/agentos")
	if len(got) != 20 {
		t.Errorf("PackageProtectedParts(bashy, internal/agentos) has %d rows, want 20 files", len(got))
	}
	for _, e := range got {
		if e.Core || e.Repo != "bashy" {
			t.Errorf("row %+v is not a bashy protected row", e)
		}
	}
}
