// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Path patterns shared by the fingerprint cache and watch mode.
//
// Sources/Inputs/Generates entries are either literal paths (a file or a
// directory, resolved against the document directory exactly as before) or
// glob patterns. A pattern containing any of `*?[` is a glob: `*` and `?` and
// character classes match within one path segment (path.Match semantics),
// while a whole segment `**` crosses segment boundaries (including zero
// segments, so `src/**` also matches `src` itself). A leading `!` marks an
// exclusion: excluded paths are subtracted from the union of the positive
// patterns. Patterns are written with forward slashes and match that way on
// every OS (filepath.ToSlash on both sides), so the same dag.md behaves the
// same on Linux, macOS and Windows.
//
// Expansion is deterministic: every expansion returns sorted forward-slash
// relative paths (or absolute slash paths for absolute patterns outside the
// document directory), deduplicated.

// splitPatterns separates exclusion patterns ("!...") from positive ones.
func splitPatterns(patterns []string) (positive, exclusions []string) {
	for _, p := range patterns {
		if strings.HasPrefix(p, "!") {
			exclusions = append(exclusions, p)
		} else {
			positive = append(positive, p)
		}
	}
	return positive, exclusions
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// absOf resolves an expansion label back to a filesystem path. Labels are
// usually slash-relative to dir; absolute patterns can produce absolute
// labels, which filepath.Join would splice onto dir and lose the root.
func absOf(dir, label string) string {
	if fs := filepath.FromSlash(label); filepath.IsAbs(fs) {
		return fs
	}
	return filepath.Join(dir, filepath.FromSlash(label))
}

// slashLabel renders abs (known to exist) as the expansion label: slash
// relative to dir when inside it, otherwise the absolute slash path.
func slashLabel(dir, abs, fallback string) string {
	if rel, err := filepath.Rel(dir, abs); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	if fallback != "" {
		return fallback
	}
	return filepath.ToSlash(abs)
}

// matchPatternRels returns the sorted slash labels matched by one positive
// pattern: literal paths via os.Stat (a missing path matches nothing), globs
// via a walk from the pattern's static prefix. Directories are included —
// callers decide whether a directory itself counts (existence) or is recursed
// (content hashing).
func matchPatternRels(dir, pattern string) []string {
	slashPat := filepath.ToSlash(pattern)
	if !hasGlobMeta(slashPat) {
		abs := filepath.Join(dir, pattern)
		if _, err := os.Stat(abs); err != nil {
			return nil
		}
		return []string{slashLabel(dir, abs, slashPat)}
	}
	absPat := pattern
	if !filepath.IsAbs(pattern) {
		absPat = filepath.Join(dir, pattern)
	}
	absSlashPat := filepath.ToSlash(absPat)
	root := walkRoot(absSlashPat)
	if root == "" {
		return nil
	}
	relMode := !filepath.IsAbs(pattern)
	var out []string
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		var name, target string
		if relMode {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return nil
			}
			name, target = filepath.ToSlash(rel), slashPat
		} else {
			name, target = filepath.ToSlash(p), absSlashPat
		}
		if name == "." {
			return nil
		}
		if matchDoublestar(target, name) {
			out = append(out, name)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// walkRoot returns where a glob walk starts: the pattern's static path prefix
// when it names an existing directory, else the deepest existing ancestor
// directory, else "" (nothing can match).
func walkRoot(absSlashPat string) string {
	prefix := absSlashPat
	if idx := strings.IndexAny(absSlashPat, "*?["); idx >= 0 {
		prefix = absSlashPat[:idx]
	}
	cand := filepath.FromSlash(prefix)
	if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
		return cand
	}
	p := cand
	for {
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
		if fi, err := os.Stat(p); err == nil {
			if fi.IsDir() {
				return p
			}
			return ""
		}
	}
}

// matchDoublestar matches slash-separated pattern against slash-separated
// name; a whole segment "**" spans zero or more segments.
func matchDoublestar(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil {
		ok = pat[0] == segs[0] // bad pattern fragment: literal compare
	}
	return ok && matchSegments(pat[1:], segs[1:])
}

// expandPatternFiles expands positive patterns to the sorted slash labels of
// every matched FILE; matched directories are recursed (like the old literal
// directory hash), so new files under a matched dir invalidate.
func expandPatternFiles(dir string, patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(label string) {
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	for _, p := range patterns {
		for _, rel := range matchPatternRels(dir, p) {
			abs := absOf(dir, rel)
			fi, err := os.Stat(abs)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				_ = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
					if err == nil && !info.IsDir() {
						add(slashLabel(dir, path, filepath.ToSlash(path)))
					}
					return nil
				})
				continue
			}
			add(rel)
		}
	}
	sort.Strings(out)
	return out
}

// exclusionSet expands exclusion patterns (still carrying their "!" prefix)
// to the set of slash labels they remove: matched paths plus, for matched
// directories, every file beneath.
func exclusionSet(dir string, exclusions []string) map[string]bool {
	set := map[string]bool{}
	for _, e := range exclusions {
		for _, rel := range matchPatternRels(dir, strings.TrimPrefix(e, "!")) {
			set[rel] = true
			if abs := absOf(dir, rel); isDir(abs) {
				_ = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
					if err == nil && !info.IsDir() {
						set[slashLabel(dir, path, filepath.ToSlash(path))] = true
					}
					return nil
				})
			}
		}
	}
	return set
}

func isDir(abs string) bool {
	fi, err := os.Stat(abs)
	return err == nil && fi.IsDir()
}

// collectSourceFiles expands Sources/Inputs patterns to the sorted slash
// labels of the files they cover: the union of the positive patterns minus
// the exclusions.
func collectSourceFiles(dir string, patterns []string) []string {
	pos, excl := splitPatterns(patterns)
	if len(pos) == 0 {
		return nil
	}
	files := expandPatternFiles(dir, pos)
	if len(excl) == 0 {
		return files
	}
	banned := exclusionSet(dir, excl)
	var out []string
	for _, f := range files {
		if !banned[f] {
			out = append(out, f)
		}
	}
	return out
}

// generatesMissing returns the first positive Generates pattern with no
// non-excluded match, or "" when every required pattern is satisfied. An
// unmatched required pattern is never a cache hit.
func generatesMissing(dir string, patterns []string) string {
	pos, excl := splitPatterns(patterns)
	if len(pos) == 0 {
		return "(no output pattern)"
	}
	var banned map[string]bool
	if len(excl) > 0 {
		banned = exclusionSet(dir, excl)
	}
	for _, g := range pos {
		matched := false
		for _, rel := range matchPatternRels(dir, g) {
			if !banned[rel] {
				matched = true
				break
			}
		}
		if !matched {
			return g
		}
	}
	return ""
}

// expansionHash deterministically hashes matched file labels plus contents.
func expansionHash(dir string, files []string) string {
	h := sha256.New()
	for _, f := range files {
		io.WriteString(h, f+"\x00")
		hashFile(h, absOf(dir, f))
	}
	return hex.EncodeToString(h.Sum(nil))
}
