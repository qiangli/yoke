package src

import (
	"sort"
	"strings"

	ygit "github.com/qiangli/yoke/git"
)

// diffText renders the unified diff from rev a to rev b in dir.
func diffText(dir, a, b string) (string, error) {
	out, err := ygit.DiffCommits(dir, a, b)
	if err != nil {
		return "", err
	}
	return out, nil
}

// parseDiffFiles lists every repo-relative path a unified diff touches,
// from its `diff --git` headers (both sides of a rename count). Headers
// are the one line per file go-git's renderer always emits — text, binary,
// mode-change, and empty-file entries alike.
func parseDiffFiles(patch string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || p == "/dev/null" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		rest := strings.TrimPrefix(line, "diff --git ")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) != 2 {
			continue
		}
		add(strings.TrimPrefix(parts[0], "a/"))
		add(strings.TrimPrefix(parts[1], "b/"))
	}
	sort.Strings(out)
	return out
}

// changedFiles resolves a and b and returns the sorted repo-relative paths
// that differ between them.
func changedFiles(dir, a, b string) ([]string, error) {
	text, err := diffText(dir, a, b)
	if err != nil {
		return nil, err
	}
	return parseDiffFiles(text), nil
}

// mergeBase resolves the first common ancestor of a and b.
func mergeBase(dir, a, b string) (string, error) {
	bases, err := ygit.MergeBase(dir, a, b)
	if err != nil {
		return "", err
	}
	if len(bases) == 0 {
		return "", &noAncestorError{a: a, b: b}
	}
	return bases[0], nil
}

type noAncestorError struct{ a, b string }

func (e *noAncestorError) Error() string {
	return "src: " + shortSHA(e.a) + " and " + shortSHA(e.b) + " have no common ancestor"
}
