package dag

import (
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// writeGraphStream emits the task graph as a bashy-graph-v1 stream (Sprint
// 446 S19), so `dag --graph | lens -t mermaid` draws it: the file node
// contains every target, and each target "requires" its prerequisites. It
// describes the file, not a run — no status, timing or cache state — so the
// bytes are a pure function of the (expanded) document. The --list --json
// envelope is unchanged.
func writeGraphStream(w io.Writer, doc *Document, file, workDir string) error {
	file = streamFile(file, workDir)
	root := ref.Locator{Scheme: "dag", Target: file}.String()
	b := stream.NewBuilder(root)
	rootAttrs := map[string]any{}
	if doc.Name != "" {
		rootAttrs["name"] = doc.Name
	}
	if d := defaultTarget(doc); d != "" {
		rootAttrs["default"] = d
	}
	b.Node(provider.Node{Ref: root, Path: file, Name: path.Base(file), Kind: "file", Type: "text/markdown", Attrs: rootAttrs})
	target := func(name string) string {
		return ref.Locator{Scheme: "dag", Target: file, Inner: []string{name}}.String()
	}
	for _, name := range doc.Order {
		t, ok := doc.Lookup(name)
		if !ok {
			continue
		}
		lang := t.Lang
		if lang == "" {
			lang = "bash"
		}
		attrs := map[string]any{}
		if t.Line > 0 {
			attrs["line"] = t.Line
		}
		if t.Desc != "" {
			attrs["desc"] = firstLine(t.Desc)
		}
		for k, v := range map[string][]string{"inputs": t.Inputs, "sources": t.Sources, "generates": t.Generates, "effects": t.Effects} {
			if len(v) > 0 {
				attrs[k] = append([]string(nil), v...)
			}
		}
		for k, v := range map[string]string{"host": t.Host, "venue": t.Venue} {
			if v != "" {
				attrs[k] = v
			}
		}
		if t.Retries > 0 {
			attrs["retries"] = t.Retries
		}
		if t.Timeout > 0 {
			attrs["timeout"] = t.Timeout.String()
		}
		r := target(t.Name)
		b.Node(provider.Node{Ref: r, Path: file, Name: t.Name, Kind: "target", Type: lang, Attrs: attrs})
		b.Edge(root, "contains", r, nil)
		for _, dep := range t.Requires {
			b.Edge(r, "requires", target(dep), nil)
		}
	}
	return b.Encode(w, "dag", "op=graph")
}

// streamFile names the dag file relative to the working directory when it
// lies under it, so the stream does not depend on where the checkout lives.
func streamFile(file, workDir string) string {
	if filepath.IsAbs(file) && workDir != "" {
		if r, err := filepath.Rel(workDir, file); err == nil && !strings.HasPrefix(r, "..") {
			file = r
		}
	}
	return filepath.ToSlash(filepath.Clean(file))
}
