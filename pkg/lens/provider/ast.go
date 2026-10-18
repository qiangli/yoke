package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/treesitter"
)

var _ Provider = (*AST)(nil)

// AST exposes top-level symbols of a source file as a read-only tree:
// ast:<file> is the file node, ast:<file>!/<symbol> a symbol node.
type AST struct {
	Root string
}

// NewAST returns an ast: provider resolving file targets under root.
func NewAST(root string) *AST { return &AST{Root: root} }

type astSymbol struct {
	id  string
	sym treesitter.Symbol
}

func (a *AST) resolve(loc ref.Locator) (rel, abs string, err error) {
	if loc.Scheme != "ast" || len(loc.Inner) > 1 || loc.ScopeKind != "" || loc.Target == "" {
		return "", "", ErrUnsupported
	}
	rel = filepath.ToSlash(filepath.Clean(loc.Target))
	if filepath.IsAbs(loc.Target) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", "", fmt.Errorf("provider: ast path %q escapes root", loc.Target)
	}
	root := a.Root
	if root == "" {
		root = "."
	}
	return rel, filepath.Join(root, filepath.FromSlash(rel)), nil
}

func (a *AST) load(ctx context.Context, loc ref.Locator) (rel string, info os.FileInfo, lang string, syms []astSymbol, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	var abs string
	if rel, abs, err = a.resolve(loc); err != nil {
		return
	}
	if info, err = os.Stat(abs); err != nil {
		return
	}
	if info.IsDir() {
		err = fmt.Errorf("provider: ast target %q is a directory", rel)
		return
	}
	lang = strings.TrimPrefix(filepath.Ext(rel), ".")
	if !treesitter.IsSupported(lang) {
		err = fmt.Errorf("provider: ast: unsupported language for %q: %w", rel, ErrUnsupported)
		return
	}
	var src []byte
	if src, err = os.ReadFile(abs); err != nil {
		return
	}
	p := treesitter.NewParser()
	tree, perr := p.Parse(ctx, src, lang)
	if perr != nil {
		err = perr
		return
	}
	raw := p.ExtractSymbols(tree, rel)
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].Line < raw[j].Line })
	counts := map[string]int{}
	for _, s := range raw {
		counts[s.Name]++
	}
	for _, s := range raw {
		id := s.Name
		if counts[s.Name] > 1 {
			id += "#" + strconv.Itoa(s.Line)
		}
		syms = append(syms, astSymbol{id: id, sym: s})
	}
	return
}

func astFileRef(rel string) string {
	return ref.Locator{Scheme: "ast", Target: rel}.String()
}

func astSymRef(rel, id string) string {
	return ref.Locator{Scheme: "ast", Target: rel, Inner: []string{id}}.String()
}

func astFileNode(rel string, info os.FileInfo, lang string, n int) Node {
	size := info.Size()
	count := uint64(n)
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           astFileRef(rel),
		Path:          rel,
		Name:          filepath.Base(rel),
		Kind:          "file",
		Type:          "ast/file",
		Size:          &size,
		Mtime:         info.ModTime().UTC().Format(time.RFC3339Nano),
		Children:      &count,
		Attrs:         map[string]any{"language": lang},
	}
}

func astSymNode(rel string, s astSymbol) Node {
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           astSymRef(rel, s.id),
		Path:          rel + "!/" + s.id,
		Name:          s.sym.Name,
		Kind:          "symbol",
		Type:          "ast/" + s.sym.Kind,
		Attrs: map[string]any{
			"symbol_kind": s.sym.Kind,
			"signature":   s.sym.Signature,
			"line":        s.sym.Line,
			"exported":    s.sym.Exported,
		},
	}
}

func astFind(syms []astSymbol, id string) (astSymbol, bool) {
	for _, s := range syms {
		if s.id == id {
			return s, true
		}
	}
	return astSymbol{}, false
}

// Stat returns the file node for ast:<file> or a symbol node for ast:<file>!/<symbol>.
func (a *AST) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	rel, info, lang, syms, err := a.load(ctx, loc)
	if err != nil {
		return Node{}, err
	}
	if len(loc.Inner) == 0 {
		return astFileNode(rel, info, lang, len(syms)), nil
	}
	s, ok := astFind(syms, loc.Inner[0])
	if !ok {
		return Node{}, fmt.Errorf("provider: ast symbol %q in %s: %w", loc.Inner[0], rel, os.ErrNotExist)
	}
	return astSymNode(rel, s), nil
}

// List returns the symbols of a file; symbols have no children.
func (a *AST) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	rel, _, _, syms, err := a.load(ctx, loc)
	if err != nil {
		return nil, err
	}
	if len(loc.Inner) != 0 {
		return nil, ErrUnsupported
	}
	nodes := make([]Node, 0, len(syms))
	for _, s := range syms {
		nodes = append(nodes, astSymNode(rel, s))
	}
	return nodes, nil
}

func (a *AST) Open(context.Context, ref.Locator) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}

// Edges returns "contains" edges from a file to its symbols, or the single
// "defined_in" edge from a symbol to its file.
func (a *AST) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	rel, _, _, syms, err := a.load(ctx, loc)
	if err != nil {
		return nil, err
	}
	if len(loc.Inner) == 0 {
		edges := make([]Edge, 0, len(syms))
		for _, s := range syms {
			edges = append(edges, Edge{
				SchemaVersion: EdgeSchema,
				Src:           astFileRef(rel),
				Rel:           "contains",
				Dst:           astSymRef(rel, s.id),
				Attrs:         map[string]any{"line": s.sym.Line},
			})
		}
		return edges, nil
	}
	s, ok := astFind(syms, loc.Inner[0])
	if !ok {
		return nil, fmt.Errorf("provider: ast symbol %q in %s: %w", loc.Inner[0], rel, os.ErrNotExist)
	}
	return []Edge{{
		SchemaVersion: EdgeSchema,
		Src:           astSymRef(rel, s.id),
		Rel:           "defined_in",
		Dst:           astFileRef(rel),
		Attrs:         map[string]any{"line": s.sym.Line},
	}}, nil
}
