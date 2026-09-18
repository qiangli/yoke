// Local search (P0b) — the other half of the find-things primitive. A unifying
// FRONT over what bashy already has: a content/filename scan (grep+find, no new
// index — a scan is not an index) and kb facts (the kb Go API). ast (treesitter)
// and graph are a documented follow-up; an agent can call those verbs directly
// today.
package search

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/scope"
)

// LocalResult is one local hit, in a uniform shape across domains.
type LocalResult struct {
	Kind string `json:"kind"` // "content" | "file" | "kb" | "route"
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

// LocalOptions configures a local search.
type LocalOptions struct {
	Dir        string // root to scan (default: cwd)
	MaxResults int    // default 40
	Domain     string // "" | "content" | "files" | "kb" ; "" = content + kb
}

// skipDirs are never descended into.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".bashy": true,
	"dist": true, "build": true, "target": true, ".cache": true, ".venv": true,
}

// Local runs a local search and returns unified results.
func Local(query string, opt LocalOptions) ([]LocalResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("search: empty local query")
	}
	max := opt.MaxResults
	if max <= 0 {
		max = 40
	}
	dir := opt.Dir
	if strings.TrimSpace(dir) == "" {
		dir, _ = os.Getwd()
	}

	// An explicit --domain (content|files|kb) is honored for back-compat. With no
	// domain, the router classifies the query into a lane and dispatches to the
	// cheapest primitive that can answer it (docs/bashy-search-design.md).
	switch strings.ToLower(strings.TrimSpace(opt.Domain)) {
	case "kb":
		return searchKB(q, max)
	case "files":
		return scanTree(dir, q, max, true)
	case "content":
		return scanTree(dir, q, max, false)
	}

	lane, term := Classify(q)
	switch lane {
	case LaneFiles:
		return scanTree(dir, term, max, true)
	case LaneKB:
		return searchKB(term, max)
	case LaneSymbol, LaneRefs:
		// Not yet answered in-process — these need ast/graph exposed as a callable
		// library (the P0.5 extraction). Until then, route: name the verb that
		// answers it so the caller (or agent) runs it directly.
		return []LocalResult{{
			Kind: "route",
			Path: laneVerb(lane),
			Text: "run `bashy " + laneVerb(lane) + " " + term + "` — " + string(lane) + " intent is a code-intel lane, not a text scan",
		}}, nil
	default: // LaneContent → content scan + kb backfill (a scan answers or narrows anything)
		out, _ := scanTree(dir, term, max, false)
		if len(out) < max {
			if kbHits, _ := searchKB(term, max-len(out)); len(kbHits) > 0 {
				out = append(out, kbHits...)
			}
		}
		return out, nil
	}
}

// scanTree walks dir matching file CONTENT (or file NAMES when byName), skipping
// the usual noise and binary files. Case-insensitive substring — a scan, not a
// regex index.
func scanTree(dir, query string, max int, byName bool) ([]LocalResult, error) {
	match := buildMatcher(query)
	var out []LocalResult
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(out) >= max {
			if len(out) >= max {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if byName {
			if match(d.Name()) {
				out = append(out, LocalResult{Kind: "file", Path: rel(dir, path)})
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 2<<20 { // skip files > 2 MB
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || bytes.IndexByte(data[:min(len(data), 512)], 0) >= 0 {
			return nil // unreadable or binary
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		ln := 0
		for sc.Scan() {
			ln++
			line := sc.Text()
			if match(line) {
				out = append(out, LocalResult{Kind: "content", Path: rel(dir, path), Line: ln, Text: strings.TrimSpace(line)})
				if len(out) >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return out, err
}

// searchKB runs the kb term search over the active scope's pages.
func searchKB(query string, max int) ([]LocalResult, error) {
	sc, err := scope.Resolve(scope.Options{
		RepoSub: kb.RepoSub,
		HostDir: func() (string, error) { return kb.DefaultDir(), nil },
	})
	if err != nil {
		return nil, nil // no scope resolvable — kb simply contributes nothing
	}
	store := kb.Open(sc.Dir())
	pages, err := store.List()
	if err != nil || len(pages) == 0 {
		return nil, nil
	}
	hits := kb.Search(pages, kb.Query{Terms: kb.Terms(query), K: max})
	out := make([]LocalResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, LocalResult{Kind: "kb", Path: h.Page.Slug + " — " + h.Page.Title})
	}
	return out, nil
}

func rel(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil {
		return r
	}
	return path
}

// buildMatcher returns a line/name predicate for a query. When the query carries
// regex metacharacters and compiles cleanly, it is a case-insensitive regex;
// otherwise (and on a bad pattern) it is a case-insensitive literal substring —
// the fast, no-surprise default. This is the interim in-process matcher; the
// design's endpoint is to delegate to the coreutils grep engine once that is
// exposed as a callable library (`grep.Search`) rather than a cobra command.
func buildMatcher(query string) func(string) bool {
	if hasRegexMeta(query) {
		if re, err := regexp.Compile("(?i)" + query); err == nil {
			return re.MatchString
		}
	}
	needle := strings.ToLower(query)
	return func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }
}

func hasRegexMeta(s string) bool {
	return strings.ContainsAny(s, `.*+?()[]{}|^$\`)
}
