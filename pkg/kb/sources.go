package kb

import (
	"bufio"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// A transfer source is a private agent-memory store on this host that
// knowledge can be distilled FROM (via the knowledge-transfer skill) into kb
// pages. The hard rule, pinned by tests: kb READS these stores and never
// writes them — every store belongs to the tool that maintains it. Probing
// is best-effort by design (count entries, no strict parsing, absence is
// normal), so this file must never grow a dependency on another tool's
// internals.

// SourceInfo describes one detected (or known-but-absent) source store.
type SourceInfo struct {
	Name    string `json:"name"`   // xfer tag suffix: claude-memory | memex | weave-memory | repo-graph
	Path    string `json:"path"`   // store location probed
	Format  string `json:"format"` // frontmatter-md | jsonl
	Present bool   `json:"present"`
	Entries int    `json:"entries,omitempty"`
	Newest  string `json:"newest,omitempty"` // RFC 3339 mtime of the newest entry
}

// DetectSources probes the known private-store layouts under home and the
// repo containing cwd. Deterministic order; absent stores are reported with
// Present=false rather than omitted or erroring.
func DetectSources(home, cwd string) []SourceInfo {
	var out []SourceInfo

	// Claude Code per-project memory: ~/.claude/projects/<id>/memory/*.md
	claudeGlob := filepath.Join(home, ".claude", "projects", "*", "memory")
	claudeDirs, _ := filepath.Glob(claudeGlob)
	sort.Strings(claudeDirs)
	found := false
	for _, d := range claudeDirs {
		if info := mdDirSource("claude-memory", d); info != nil {
			out = append(out, *info)
			found = true
		}
	}
	if !found {
		out = append(out, SourceInfo{Name: "claude-memory", Path: claudeGlob, Format: "frontmatter-md"})
	}

	// ycode memex: global + project-scoped frontmatter-md dirs.
	memexGlobal := filepath.Join(home, ".agents", "ycode", "memory")
	if info := mdDirSource("memex", memexGlobal); info != nil {
		out = append(out, *info)
	} else {
		out = append(out, SourceInfo{Name: "memex", Path: memexGlobal, Format: "frontmatter-md"})
	}
	repoRoot := ""
	if cwd != "" {
		repoRoot = repoRootOf(cwd)
	}
	if repoRoot != "" {
		memexProject := filepath.Join(repoRoot, ".agents", "ycode", "memory")
		if info := mdDirSource("memex", memexProject); info != nil {
			out = append(out, *info)
		}
	}

	// weave campaign memory: ~/.bashy/weave/<tag>/memory.jsonl
	weaveGlob := filepath.Join(home, ".bashy", "weave", "*", "memory.jsonl")
	weaveFiles, _ := filepath.Glob(weaveGlob)
	sort.Strings(weaveFiles)
	found = false
	for _, f := range weaveFiles {
		if info := jsonlSource("weave-memory", f); info != nil {
			out = append(out, *info)
			found = true
		}
	}
	if !found {
		out = append(out, SourceInfo{Name: "weave-memory", Path: weaveGlob, Format: "jsonl"})
	}

	// repo relation log: <repoRoot>/docs/kb/graph.jsonl
	if repoRoot != "" {
		contrib := RepoContribPath(repoRoot)
		if info := jsonlSource("repo-graph", contrib); info != nil {
			out = append(out, *info)
		} else {
			out = append(out, SourceInfo{Name: "repo-graph", Path: contrib, Format: "jsonl"})
		}
	}

	return out
}

// mdDirSource probes a directory of frontmatter-markdown memory files.
// MEMORY.md is an index, not an entry — the same rule its writers follow.
func mdDirSource(name, dir string) *SourceInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	count := 0
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		count++
		if fi, err := e.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	if count == 0 {
		return nil
	}
	return &SourceInfo{
		Name: name, Path: dir, Format: "frontmatter-md", Present: true,
		Entries: count, Newest: newest.UTC().Format(time.RFC3339),
	}
}

// jsonlSource probes an append-only JSONL store; entries = non-blank lines.
func jsonlSource(name, path string) *SourceInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			count++
		}
	}
	// Best-effort probe: a scan error (e.g. an oversized line) keeps the
	// lines counted so far rather than hiding a store that plainly exists.
	if sc.Err() != nil && count == 0 {
		return nil
	}
	if count == 0 {
		return nil
	}
	info := &SourceInfo{Name: name, Path: path, Format: "jsonl", Present: true, Entries: count}
	if fi, err := f.Stat(); err == nil {
		info.Newest = fi.ModTime().UTC().Format(time.RFC3339)
	}
	return info
}

// --- memex source records --------------------------------------------------

// memexSource is the xfer tag suffix and Source.Tool stamp for records moved
// out of a ycode memex store.
const memexSource = "memex"

// memexMemory is the subset of a ycode memex memory record kb needs to move a
// memory into the agent ring. kb reimplements the frontmatter-md parse rather
// than importing ycode's memex package: pkg/kb is an import leaf (scope/bus/git
// only), and the on-disk frontmatter shape — not the Go type — is the stable
// contract. Fields mirror memory.Memory{Name,Description,Type,Scope,Content,
// Tags,ContentHash,SupersededBy,ValidUntil}.
type memexMemory struct {
	Name         string
	Description  string
	Type         string
	Scope        string
	Content      string
	Tags         []string
	ContentHash  string     // md5(normalize(content)); computed when absent on disk
	SupersededBy string     // NAME of the memory that replaced this one
	ValidUntil   *time.Time // temporal validity; a past value means expired
}

// readMemexDir parses every frontmatter-md memory file in dir into memexMemory
// records, in deterministic (name-sorted) order. MEMORY.md is an index, not an
// entry — skipped, the same rule mdDirSource follows. A missing dir returns no
// records and no error (absence is normal); a corrupt file is skipped.
func readMemexDir(dir string) ([]*memexMemory, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []*memexMemory
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if m := parseMemexMemory(string(b)); m != nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// parseMemexMemory parses one memex memory file: a `key: value` YAML-ish
// frontmatter block over a free markdown body, exactly the shape memex's own
// store writes. Unknown keys are ignored (kb reads the world's files, it does
// not lint them). The content hash is computed from the body when the file did
// not carry one, matching memex's md5(normalize(content)).
func parseMemexMemory(data string) *memexMemory {
	m := &memexMemory{}
	if !strings.HasPrefix(data, "---\n") {
		m.Content = data
		return finishMemexMemory(m)
	}
	endIdx := strings.Index(data[4:], "\n---\n")
	if endIdx == -1 {
		m.Content = data
		return finishMemexMemory(m)
	}
	frontmatter := data[4 : 4+endIdx]
	m.Content = strings.TrimSpace(data[4+endIdx+5:])
	for line := range strings.SplitSeq(frontmatter, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			m.Name = value
		case "description":
			m.Description = value
		case "type":
			m.Type = value
		case "scope":
			m.Scope = value
		case "tags":
			if value != "" {
				m.Tags = splitMemexTags(value)
			}
		case "content_hash":
			m.ContentHash = value
		case "superseded_by":
			m.SupersededBy = value
		case "valid_until":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				m.ValidUntil = &t
			}
		}
	}
	return finishMemexMemory(m)
}

func finishMemexMemory(m *memexMemory) *memexMemory {
	if m.ContentHash == "" {
		m.ContentHash = memexContentHash(m.Content)
	}
	return m
}

// splitMemexTags splits memex's comma-joined tag string, dropping blanks.
func splitMemexTags(s string) []string {
	var out []string
	for t := range strings.SplitSeq(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// memexContentHash reproduces memex's dedup hash: md5 over case-folded,
// whitespace-collapsed content. Deterministic across runs, so a second
// transfer of the same store re-derives the same hashes and moves nothing.
func memexContentHash(content string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(content)), " ")
	return fmt.Sprintf("%x", md5.Sum([]byte(normalized)))
}

// TransferredCounts scans live pages for xfer:<source> tags — the
// idempotence marker the knowledge-transfer skill stamps on every
// transferred page — and returns pages-per-source. Superseded pages are
// excluded: a superseded transfer no longer represents that source.
func TransferredCounts(pages []*Page) map[string]int {
	out := map[string]int{}
	for _, p := range pages {
		if p.Status == StatusSuperseded {
			continue
		}
		for _, t := range p.Tags {
			if rest, ok := strings.CutPrefix(strings.ToLower(t), "xfer:"); ok && rest != "" {
				out[rest]++
			}
		}
	}
	return out
}

// --- sources verb ----------------------------------------------------------

func newSourcesCmd(dir *string) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "Detect private agent-memory stores on this host (transfer sources; read-only)",
		Long: `Probe the known private-memory store layouts on this host — the SOURCES an
agent distills from when transferring knowledge into the kb (see the
knowledge-transfer skill: bashy skill show knowledge-transfer).

Read-only and best-effort: kb reads these stores, it NEVER writes them;
absence is normal, not an error. Per store: path, format, entry count,
newest-entry time, and how many live kb pages already carry its
xfer:<source> tag (the idempotence marker — what this source has already
contributed).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				home = ""
			}
			cwd, _ := os.Getwd()
			sources := DetectSources(home, cwd)
			store := Open(*dir)
			pages, err := store.List()
			if err != nil {
				return err
			}
			xfer := TransferredCounts(pages)
			out := c.OutOrStdout()
			if jsonOut {
				payload := struct {
					Sources     []SourceInfo   `json:"sources"`
					Transferred map[string]int `json:"transferred,omitempty"`
				}{Sources: sources, Transferred: xfer}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}
			writeSources(out, sources, xfer)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "JSON output")
	return cmd
}

// writeSourcesSummary prints one line per source NAME (stores aggregated) —
// the compact form `kb transfer` embeds so the checklist stays scannable;
// `kb sources` prints the full per-store detail.
func writeSourcesSummary(out io.Writer, sources []SourceInfo, xfer map[string]int) {
	type agg struct {
		stores, entries int
		newest          string
	}
	byName := map[string]*agg{}
	var order []string
	for _, s := range sources {
		a, ok := byName[s.Name]
		if !ok {
			a = &agg{}
			byName[s.Name] = a
			order = append(order, s.Name)
		}
		if s.Present {
			a.stores++
			a.entries += s.Entries
			if s.Newest > a.newest {
				a.newest = s.Newest
			}
		}
	}
	for _, name := range order {
		a := byName[name]
		if a.stores == 0 {
			fmt.Fprintf(out, "  %-13s (not present)\n", name)
			continue
		}
		age := ""
		if a.newest != "" {
			age = "  newest " + a.newest
		}
		fmt.Fprintf(out, "  %-13s %d entries in %d store(s)%s  (detail: bashy kb sources)\n", name, a.entries, a.stores, age)
	}
	if len(xfer) > 0 {
		names := make([]string, 0, len(xfer))
		for n := range xfer {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprint(out, "  already transferred (live pages tagged xfer:<source>):")
		for _, n := range names {
			fmt.Fprintf(out, " %s=%d", n, xfer[n])
		}
		fmt.Fprintln(out)
	}
}

func writeSources(out io.Writer, sources []SourceInfo, xfer map[string]int) {
	for _, s := range sources {
		if !s.Present {
			fmt.Fprintf(out, "%-13s (not present)  %s\n", s.Name, s.Path)
			continue
		}
		age := ""
		if s.Newest != "" {
			age = "  newest " + s.Newest
		}
		fmt.Fprintf(out, "%-13s %d entries (%s)%s  %s\n", s.Name, s.Entries, s.Format, age, s.Path)
	}
	if len(xfer) > 0 {
		names := make([]string, 0, len(xfer))
		for n := range xfer {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprint(out, "already transferred (live pages tagged xfer:<source>):")
		for _, n := range names {
			fmt.Fprintf(out, " %s=%d", n, xfer[n])
		}
		fmt.Fprintln(out)
	}
}
