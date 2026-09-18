package weave

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const weaveOutsideWorkspacePathsMax = 20

var (
	weaveANSIEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	// PTY logs usually surround paths with whitespace, quotes, parentheses,
	// or key=value punctuation. Requiring such a boundary avoids treating the
	// //host portion of URLs as a filesystem path.
	weaveAbsolutePath = regexp.MustCompile("(?:^|[\\s\"'`(<>=])(/[^\\x00-\\x20\"'`<>|]+)")
)

// weaveOutsideWorkspacePaths extracts absolute Unix paths from a worker log
// tail and returns those lexically outside workspace. This is advisory: logs
// are prose, so a match is not evidence that the worker read or wrote the path.
func weaveOutsideWorkspacePaths(logTail, workspace string) []string {
	if logTail == "" || !filepath.IsAbs(workspace) {
		return nil
	}
	logTail = weaveANSIEscape.ReplaceAllString(logTail, "")
	workspace = filepath.Clean(workspace)
	seen := map[string]bool{}
	for _, match := range weaveAbsolutePath.FindAllStringSubmatch(logTail, -1) {
		if len(match) < 2 {
			continue
		}
		path := strings.TrimRight(match[1], ".,;:!?)]}")
		path = filepath.Clean(path)
		rel, err := filepath.Rel(workspace, path)
		if err != nil || rel == "." {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			seen[path] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > weaveOutsideWorkspacePathsMax {
		paths = paths[:weaveOutsideWorkspacePathsMax]
	}
	return paths
}

func weaveOutsideWorkspaceWarning(it *weaveItem) string {
	if it == nil || len(it.OutsideWorkspacePaths) == 0 {
		return ""
	}
	return fmt.Sprintf("weave: advisory run #%d: worker referenced paths outside its workspace: %s\n",
		it.ID, strings.Join(it.OutsideWorkspacePaths, ", "))
}

func weavePrintOutsideWorkspaceRefs(w io.Writer, items []*weaveItem) {
	for _, it := range items {
		fmt.Fprint(w, weaveOutsideWorkspaceWarning(it))
	}
}
