// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package handoff

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// RepoMembers resolves the project members (root + sibling repos + nested modules).
//
// Algorithm:
//   - Start with root.
//   - Walk up from root to find the nearest enclosing go.work (stop at filesystem root).
//   - If found:
//   - Include nested modules inside root that appear in go.work.
//   - Include each go.work use dir whose go.mod module path is required
//     (require directive, direct or indirect) by root's go.mod.
//     Use dirs of other siblings' nested modules count only if root requires
//     their module path.
//   - Skip directories that do not exist or lack a matching required module.
//   - If no go.work is found, fall back to scanning root's go.mod for "../" replace directives.
//   - De-duplicate, clean paths, root first, rest sorted.
func RepoMembers(root string) []string {
	root = filepath.Clean(root)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}

	var candidates []string
	workFile := findEnclosingGoWork(absRoot)
	if workFile != "" {
		candidates = resolveGoWorkMembers(absRoot, workFile)
	} else {
		candidates = scanGoModReplaces(absRoot)
	}

	seen := map[string]bool{absRoot: true, root: true}
	var rest []string
	for _, c := range candidates {
		clean := filepath.Clean(c)
		if !seen[clean] {
			seen[clean] = true
			rest = append(rest, clean)
		}
	}
	sort.Strings(rest)
	return append([]string{root}, rest...)
}

// findEnclosingGoWork walks up from root to find the nearest enclosing go.work.
// Returns empty string if not found before reaching the filesystem root.
func findEnclosingGoWork(root string) string {
	dir := filepath.Clean(root)
	for {
		candidate := filepath.Join(dir, "go.work")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// resolveGoWorkMembers reads the go.work file using modfile.ParseWork,
// and filters use directories according to the scoping rule relative to root:
//   - nested modules inside root that appear in go.work are included;
//   - other use dirs (siblings or nested modules of siblings) are included only
//     if their go.mod module path is required (direct or indirect) by root's go.mod.
func resolveGoWorkMembers(root, workFile string) []string {
	data, err := os.ReadFile(workFile)
	if err != nil {
		return nil
	}
	wf, err := modfile.ParseWork(workFile, data, nil)
	if err != nil {
		return nil
	}

	workDir := filepath.Dir(workFile)
	requiredMods := readRootRequiredModules(root)

	var members []string
	for _, u := range wf.Use {
		if u == nil || u.Path == "" {
			continue
		}
		p := filepath.FromSlash(u.Path)
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		p = filepath.Clean(p)

		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		if p == root {
			continue
		}

		if isSubdir(p, root) {
			// Nested module inside root that appears in go.work.
			members = append(members, p)
			continue
		}

		// Sibling or sibling's nested module: include only if root's go.mod
		// requires its module path.
		modPath := readModulePath(p)
		if modPath != "" && requiredMods[modPath] {
			members = append(members, p)
		}
	}
	return members
}

// isSubdir reports whether child is a descendant subdirectory of parent.
func isSubdir(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// parseGoMod parses a go.mod file, attempting strict Parse first and falling
// back to ParseLax if needed.
func parseGoMod(path string, data []byte) *modfile.File {
	if f, err := modfile.Parse(path, data, nil); err == nil && f != nil {
		return f
	}
	if f, err := modfile.ParseLax(path, data, nil); err == nil && f != nil {
		return f
	}
	return nil
}

// readRootRequiredModules reads root's go.mod and returns the set of module paths
// required by root (both direct and indirect).
func readRootRequiredModules(root string) map[string]bool {
	gmPath := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(gmPath)
	if err != nil {
		return nil
	}
	f := parseGoMod(gmPath, data)
	if f == nil {
		return nil
	}
	reqs := make(map[string]bool, len(f.Require))
	for _, r := range f.Require {
		if r != nil && r.Mod.Path != "" {
			reqs[r.Mod.Path] = true
		}
	}
	return reqs
}

// readModulePath reads the module path declared in dir's go.mod file.
func readModulePath(dir string) string {
	gmPath := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(gmPath)
	if err != nil {
		return ""
	}
	f := parseGoMod(gmPath, data)
	if f == nil || f.Module == nil {
		return ""
	}
	return f.Module.Mod.Path
}

// scanGoModReplaces parses "../" replace targets from root's go.mod file
// using modfile.Parse (fallback when there is no go.work).
func scanGoModReplaces(root string) []string {
	gmPath := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(gmPath)
	if err != nil {
		return nil
	}
	f := parseGoMod(gmPath, data)
	if f == nil {
		return nil
	}
	var members []string
	for _, rep := range f.Replace {
		if rep == nil {
			continue
		}
		targetPath := filepath.ToSlash(rep.New.Path)
		if targetPath != ".." && !strings.HasPrefix(targetPath, "../") {
			continue
		}
		abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rep.New.Path)))
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			members = append(members, abs)
		}
	}
	return members
}
