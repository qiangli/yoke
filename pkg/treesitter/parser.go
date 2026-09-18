package treesitter

import (
	"context"
	"fmt"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Tree represents a parsed source file.
type Tree struct {
	Root     *gotreesitter.Node
	Source   []byte
	Lang     string
	language *gotreesitter.Language
	raw      *gotreesitter.Tree
}

// Parser wraps tree-sitter parsing for multiple languages.
type Parser struct {
	parsers map[string]*gotreesitter.Parser
	taggers map[string]*gotreesitter.Tagger
}

// NewParser creates a new tree-sitter parser.
func NewParser() *Parser {
	return &Parser{
		parsers: make(map[string]*gotreesitter.Parser),
		taggers: make(map[string]*gotreesitter.Tagger),
	}
}

// getParser returns a cached parser for the given language, creating one if needed.
func (p *Parser) getParser(lang string, language *gotreesitter.Language) *gotreesitter.Parser {
	if parser, ok := p.parsers[lang]; ok {
		return parser
	}
	parser := gotreesitter.NewParser(language)
	p.parsers[lang] = parser
	return parser
}

// Parse parses source code in the given language and returns the AST.
func (p *Parser) Parse(_ context.Context, source []byte, lang string) (*Tree, error) {
	language := GetLanguage(lang)
	if language == nil {
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}

	parser := p.getParser(lang, language)
	tree, err := parser.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", lang, err)
	}

	return &Tree{
		Root:     tree.RootNode(),
		Source:   source,
		Lang:     lang,
		language: language,
		raw:      tree,
	}, nil
}

// ExtractSymbols returns top-level definition symbols using the upstream
// tree-sitter Tagger. Signature is the real declaration header from source
// (everything from the start of the tagged span up to the body delimiter),
// not a synthesized "<kind> <name>" string.
//
// Supplemental walkers (walkSupplementalSymbols) cover languages where the
// upstream tagger has gaps: TypeScript type-alias declarations and Ruby
// (which has no upstream tags query at all today).
func (p *Parser) ExtractSymbols(tree *Tree, file string) []Symbol {
	if tree == nil || tree.raw == nil || len(tree.Source) == 0 {
		return nil
	}
	out := []Symbol{}
	seen := make(map[uint32]bool)
	if tagger := p.taggerFor(tree.Lang, tree.language); tagger != nil {
		tags := tagger.TagTree(tree.raw)
		for _, t := range tags {
			if !strings.HasPrefix(t.Kind, "definition.") {
				continue
			}
			if seen[t.Range.StartByte] {
				continue
			}
			seen[t.Range.StartByte] = true

			sig := extractHeader(tree.Source, t.Range)
			kind := refineKind(tree.Lang, t.Kind, sig)
			if kind == "" {
				continue
			}
			out = append(out, Symbol{
				Name:      t.Name,
				Kind:      kind,
				Signature: sig,
				File:      file,
				Line:      int(t.Range.StartPoint.Row) + 1,
				Exported:  isExported(tree.Lang, t.Name, sig),
			})
		}
	}
	out = append(out, walkSupplementalSymbols(tree, file, seen)...)
	return out
}

// walkSupplementalSymbols handles symbol kinds the upstream tagger misses.
// Two known gaps as of upstream gotreesitter v0:
//
//   - TypeScript: `type X = ...` (type_alias_declaration node) is not in
//     the tags query and would otherwise be silently dropped.
//   - Ruby: ResolveTagsQuery returns "" so the tagger is nil and every
//     symbol is missed. The walker emits methods, classes, and modules.
//
// `seen` is shared with the tagger pass to dedupe by start-byte.
func walkSupplementalSymbols(tree *Tree, file string, seen map[uint32]bool) []Symbol {
	var (
		nodeTypes map[string]string // tree-sitter node kind → ycode Symbol.Kind
	)
	switch tree.Lang {
	case "typescript":
		nodeTypes = map[string]string{"type_alias_declaration": "type"}
	case "ruby":
		nodeTypes = map[string]string{
			"method":           "func",
			"singleton_method": "func",
			"class":            "class",
			"module":           "class",
		}
	default:
		return nil
	}

	var out []Symbol
	WalkNodes(tree.Root, tree.language, func(node *gotreesitter.Node) bool {
		kind, ok := nodeTypes[node.Type(tree.language)]
		if !ok {
			return true
		}
		startByte := uint32(node.StartByte())
		if seen[startByte] {
			return true
		}
		nameNode := node.ChildByFieldName("name", tree.language)
		if nameNode == nil {
			return true
		}
		seen[startByte] = true
		name := nodeText(nameNode, tree.Source)
		sig := extractHeader(tree.Source, gotreesitter.Range{
			StartByte:  startByte,
			EndByte:    uint32(node.EndByte()),
			StartPoint: node.StartPoint(),
			EndPoint:   node.EndPoint(),
		})
		out = append(out, Symbol{
			Name:      name,
			Kind:      kind,
			Signature: sig,
			File:      file,
			Line:      int(node.StartPoint().Row) + 1,
			Exported:  isExported(tree.Lang, name, sig),
		})
		return true
	})
	return out
}

// taggerFor returns a cached Tagger for the given language, building one
// from the upstream registry's tags query if needed. Returns nil if the
// language has no tags query available.
func (p *Parser) taggerFor(lang string, language *gotreesitter.Language) *gotreesitter.Tagger {
	if t, ok := p.taggers[lang]; ok {
		return t
	}
	canonical := lang
	if c, ok := langAliases[lang]; ok {
		canonical = c
	}
	entry := grammars.DetectLanguageByName(canonical)
	if entry == nil {
		return nil
	}
	query := grammars.ResolveTagsQuery(*entry)
	if strings.TrimSpace(query) == "" {
		return nil
	}
	t, err := gotreesitter.NewTagger(language, query)
	if err != nil {
		return nil
	}
	p.taggers[lang] = t
	return t
}

// ExtractSymbols extracts top-level symbols from a parsed tree.
//
// Convenience wrapper around (*Parser).ExtractSymbols for callers that
// don't already hold a Parser. Allocates a one-shot Parser; bulk callers
// should use the method form to share Tagger compilation across files.
func ExtractSymbols(tree *Tree, file string) []Symbol {
	return NewParser().ExtractSymbols(tree, file)
}

// WalkNodes calls fn for every node in the tree (depth-first).
func WalkNodes(node *gotreesitter.Node, lang *gotreesitter.Language, fn func(*gotreesitter.Node) bool) {
	if node == nil {
		return
	}
	if !fn(node) {
		return
	}
	for i := 0; i < node.ChildCount(); i++ {
		WalkNodes(node.Child(i), lang, fn)
	}
}

// nodeText extracts the source text for a node.
func nodeText(node *gotreesitter.Node, source []byte) string {
	return node.Text(source)
}

// extractHeader returns the declaration header text — the source slice from
// the start of the tagged range up to the first body delimiter, trimmed.
// Brace languages stop at '{'; def-style languages stop at the first newline
// (with a trailing ':' stripped for Python).
func extractHeader(source []byte, r gotreesitter.Range) string {
	start, end := int(r.StartByte), int(r.EndByte)
	if start < 0 || start >= len(source) {
		return ""
	}
	if end > len(source) {
		end = len(source)
	}
	s := source[start:end]
	cut := len(s)
	for i, b := range s {
		if b == '{' || b == '\n' {
			cut = i
			break
		}
	}
	line := strings.TrimSpace(string(s[:cut]))
	line = strings.TrimRight(line, ":")
	return strings.TrimSpace(line)
}

// refineKind maps a Tagger "definition.*" kind to ycode's kind taxonomy
// (func, method, type, interface, class, const, var). Tagger lumps structs,
// enums, traits, and Go interfaces into "definition.type"; this restores
// "interface" for Go interface declarations and Rust traits by inspecting
// the signature.
func refineKind(lang, taggerKind, signature string) string {
	switch strings.TrimPrefix(taggerKind, "definition.") {
	case "function":
		return "func"
	case "method":
		return "method"
	case "class":
		return "class"
	case "interface":
		return "interface"
	case "constructor":
		return "func"
	case "constant":
		return "const"
	case "variable":
		return "var"
	case "type":
		s := strings.TrimSpace(signature)
		switch lang {
		case "go":
			// "type Handler interface" / "type Handler interface{...}"
			if strings.Contains(s, " interface") {
				return "interface"
			}
		case "rust":
			t := strings.TrimPrefix(s, "pub ")
			t = strings.TrimPrefix(t, "pub(crate) ")
			if strings.HasPrefix(t, "trait ") {
				return "interface"
			}
		}
		return "type"
	}
	return ""
}

// isExported applies language-specific visibility rules to a captured
// declaration. JS/TS/Java default to true (the upstream tags query has no
// visibility info; the previous extractor did the same).
func isExported(lang, name, signature string) bool {
	switch lang {
	case "go":
		return name != "" && name[0] >= 'A' && name[0] <= 'Z'
	case "python", "ruby":
		return !strings.HasPrefix(name, "_")
	case "rust":
		s := strings.TrimSpace(signature)
		return strings.HasPrefix(s, "pub ") || strings.HasPrefix(s, "pub(")
	default:
		return true
	}
}
