package astcmd

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/treesitter"
)

// The tree-shaped ast verbs (symbols, search, refs, map) emit the
// bashy-graph-v1 stream under --json (Sprint 446 S19), so
// `ast symbols pkg --json | lens -t outline` works. Identity is
// ast:ROOT!FILE!NAME!LINE — ROOT as the operand was spelled, FILE relative to
// it — so the bytes do not depend on where the checkout lives.
// `ast query` keeps its own match-list JSON: captures are not symbols.

const streamProvider = "ast"

// astStream is one ast result being framed.
type astStream struct {
	b    *stream.Builder
	root string // the operand, cleaned and slash-separated
	base string // absolute directory FILE paths are relative to
}

func newAstStream(operand, abs string) *astStream {
	root := filepath.ToSlash(filepath.Clean(operand))
	base := abs
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		base = filepath.Dir(abs)
	}
	s := &astStream{b: stream.NewBuilder(locator(root)), root: root, base: base}
	s.b.Node(provider.Node{Ref: locator(root), Path: root, Name: path.Base(root), Kind: "directory", Type: "inode/directory"})
	return s
}

func locator(root string, inner ...string) string {
	return ref.Locator{Scheme: "ast", Target: root, Inner: inner}.String()
}

// rel maps an engine path (absolute, or already relative to base) to FILE.
func (s *astStream) rel(file string) string {
	if filepath.IsAbs(file) {
		if r, err := filepath.Rel(s.base, file); err == nil && !strings.HasPrefix(r, "..") {
			file = r
		}
	}
	return filepath.ToSlash(file)
}

// file adds FILE under the root and returns its ref.
func (s *astStream) file(file string) string {
	r := locator(s.root, file)
	if !s.b.Has(r) {
		typ := languageFromPath(file)
		if typ == "" {
			typ = "unknown"
		}
		s.b.Node(provider.Node{Ref: r, Path: file, Name: path.Base(file), Kind: "file", Type: typ})
		s.b.Edge(locator(s.root), "contains", r, nil)
	}
	return r
}

// symbol adds one symbol definition under its file and returns its ref.
func (s *astStream) symbol(file, name, kind, signature string, line int, exported bool) string {
	attrs := map[string]any{"line": line, "exported": exported}
	if signature != "" {
		attrs["signature"] = signature
	}
	return s.site(file, name, line, "symbol", kind, attrs)
}

// site adds the node FILE!NAME!LINE (a definition or a reference) under its
// file and returns its ref.
func (s *astStream) site(file, name string, line int, kind, typ string, attrs map[string]any) string {
	file = s.rel(file)
	fr := s.file(file)
	r := locator(s.root, file, name, strconv.Itoa(line))
	if typ == "" {
		typ = "unknown"
	}
	s.b.Node(provider.Node{Ref: r, Path: file, Name: name, Kind: kind, Type: typ, Attrs: attrs})
	s.b.Edge(fr, "contains", r, nil)
	return r
}

func (s *astStream) treeSymbol(sym treesitter.Symbol) {
	s.symbol(sym.File, sym.Name, sym.Kind, sym.Signature, sym.Line, sym.Exported)
}

// target is a queried symbol the engine resolved by name only: it hangs off
// the root, not off a file.
func (s *astStream) target(name string) string {
	r := locator(s.root, name)
	s.b.Node(provider.Node{Ref: r, Path: s.root, Name: name, Kind: "symbol", Type: "query"})
	s.b.Edge(locator(s.root), "contains", r, nil)
	return r
}

func (s *astStream) write(rc *tool.RunContext, name string, q url.Values) int {
	if err := s.b.Encode(rc.Out, streamProvider, q.Encode()); err != nil {
		fmt.Fprintf(rc.Err, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// symbolFromAddress maps one consumer address (`ast refs -`) to the symbol
// name to look up, or "" for an address that names no symbol (a stream's
// root or file nodes). An ast: locator names a symbol as FILE!NAME!LINE (or
// a bare queried NAME), a graph: locator as its last element (a gfy label,
// call parentheses and receiver dropped), and anything else is the symbol.
func symbolFromAddress(a string) string {
	loc, err := ref.ParseLocator(a)
	if err != nil || !strings.HasPrefix(a, loc.Scheme+":") {
		return a
	}
	var name string
	switch loc.Scheme {
	case "ast":
		switch len(loc.Inner) {
		case 0:
			return ""
		case 1:
			name = loc.Inner[0]
		default:
			return loc.Inner[1]
		}
	case "graph":
		if len(loc.Inner) == 0 {
			return ""
		}
		name = strings.TrimSuffix(loc.Inner[len(loc.Inner)-1], "()")
		if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 && languageFromPath(name) == "" {
			name = name[i+1:]
		}
	default:
		return a
	}
	if languageFromPath(name) != "" {
		return "" // a file node
	}
	return name
}
