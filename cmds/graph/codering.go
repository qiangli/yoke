package graphcmd

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/gfy/pkg/search"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/codegraph"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/recall"
	"github.com/qiangli/yoke/pkg/repomap"
)

const (
	codeRingDefaultDepth = 1
	codeRingDefaultLimit = 40
)

// CodeRing is the code form: a read-only view over the bashy graph cache plus
// a budgeted repomap. It is a repo reader, not a stored kb ring.
type CodeRing struct {
	RepoRoot string
}

// NewCodeRing returns the code-form reader bashy can inject into kb context.
func NewCodeRing(repoRoot string) recall.Reader {
	return CodeRing{RepoRoot: repoRoot}
}

func (CodeRing) Ring() string    { return recall.RingRepo }
func (CodeRing) Forms() []string { return []string{kb.FormCode} }

func (r CodeRing) Recall(q recall.Query) ([]recall.Hit, error) {
	root := strings.TrimSpace(r.RepoRoot)
	if root == "" {
		root = "."
	}
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   root,
		FS:    tool.NewLocalFS(),
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: ioDiscard{}, Err: ioDiscard{}},
	}
	gc, err := loadOrBuild(rc, root, false, true)
	if err != nil {
		return nil, fmt.Errorf("code: open %s: %w", filepath.Join(root, cacheRel), err)
	}

	query := strings.TrimSpace(q.Text)
	hits := graphCodeHits(root, gc, query, codeRingDefaultDepth, codeRingDefaultLimit)
	hits = append(hits, repomapCodeHits(root, q)...)
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	return hits, nil
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func graphCodeHits(root string, gc *codegraph.GraphContext, query string, depth, limit int) []recall.Hit {
	if query == "" {
		query = "code"
	}
	if gc == nil || gc.Graph == nil {
		return nil
	}
	results := search.ScoreNodes(gc.Graph, query)
	if len(results) > 5 {
		results = results[:5]
	}
	out := make([]recall.Hit, 0, len(results))
	for i, result := range results {
		visited, edges := gc.Graph.BFS([]string{result.ID}, depth)
		total := len(visited)
		if limit > 0 && len(visited) > limit {
			visited = visited[:limit]
		}
		nodes, sgEdges := subgraphView(gc.Graph, visited, edges)
		label := nodeLabel(gc.Graph, result.ID)
		out = append(out, recall.Hit{
			Ring: recall.RingRepo, Kind: "code", ID: "code:" + result.ID, Form: kb.FormCode,
			Cue:   "code " + label,
			Gist:  codeGist(gc.Graph.NodeAttrs(result.ID), total, len(sgEdges)),
			Full:  codeSubgraphText(label, nodes, sgEdges),
			Score: float64(len(results)-i) + result.Score,
			Why:   []string{"codegraph", "depth 1", "limit 40"},
			Source: []recall.SourceRef{{
				URI: "file://" + filepath.Join(root, cacheRel),
			}},
		})
	}
	return out
}

func repomapCodeHits(root string, q recall.Query) []recall.Hit {
	opts := repomap.DefaultOptions()
	opts.MaxTokens = max(80, q.Budget/2)
	opts.MaxFiles = codeRingDefaultLimit
	opts.RelevanceQuery = q.Text
	opts.ChatContextFiles = normalizeRepoFiles(root, q.Files)
	rm, err := repomap.Generate(root, opts)
	if err != nil || rm == nil {
		return nil
	}
	out := make([]recall.Hit, 0, len(rm.Entries))
	for i, entry := range rm.Entries {
		if len(entry.Symbols) == 0 {
			continue
		}
		ref := "code:file:" + entry.Path
		out = append(out, recall.Hit{
			Ring: recall.RingRepo, Kind: "code", ID: ref, Form: kb.FormCode,
			Cue:   "code file " + entry.Path,
			Gist:  repomapGist(entry),
			Full:  repomapEntryText(entry),
			Score: 0.5 + entry.Score - float64(i)*0.001,
			Why:   []string{"repomap", "relevance query", "chat files boost"},
			Source: []recall.SourceRef{{
				URI: "file://" + filepath.Join(root, entry.Path),
			}},
		})
	}
	return out
}

func normalizeRepoFiles(root string, files []string) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		if filepath.IsAbs(file) {
			if rel, err := filepath.Rel(root, file); err == nil {
				file = rel
			}
		}
		out = append(out, filepath.ToSlash(file))
	}
	return out
}

func codeGist(attrs map[string]any, nodes, edges int) string {
	kind := edgeStr(attrs, "file_type")
	source := edgeStr(attrs, "source_file")
	var parts []string
	if kind != "" {
		parts = append(parts, kind)
	}
	if source != "" {
		parts = append(parts, source)
	}
	parts = append(parts, fmt.Sprintf("impact view: %d nodes, %d edges", nodes, edges))
	return strings.Join(parts, "; ")
}

func codeSubgraphText(label string, nodes []sgNode, edges []sgEdge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Code graph around %s\n", label)
	if len(nodes) > 0 {
		b.WriteString("Nodes:\n")
		for _, n := range nodes {
			fmt.Fprintf(&b, "- %s (%s)\n", n.Label, n.FileType)
		}
	}
	if len(edges) > 0 {
		b.WriteString("Edges:\n")
		for _, e := range edges {
			fmt.Fprintf(&b, "- %s -[%s]-> %s\n", e.Source, e.Relation, e.Target)
		}
	}
	return strings.TrimSpace(b.String())
}

func repomapGist(entry repomap.FileEntry) string {
	names := make([]string, 0, min(len(entry.Symbols), 6))
	for _, sym := range entry.Symbols {
		if sym.Name == "" {
			continue
		}
		names = append(names, sym.Name)
		if len(names) >= 6 {
			break
		}
	}
	if len(names) == 0 {
		return "repo map symbols"
	}
	return "symbols: " + strings.Join(names, ", ")
}

func repomapEntryText(entry repomap.FileEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", entry.Path)
	for _, sym := range entry.Symbols {
		if sym.Signature != "" {
			fmt.Fprintf(&b, "- %s\n", sym.Signature)
		} else {
			fmt.Fprintf(&b, "- %s %s\n", sym.Kind, sym.Name)
		}
	}
	return strings.TrimSpace(b.String())
}
