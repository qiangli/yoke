package treesitter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/odvcencio/gotreesitter"
)

// Analyze performs impact analysis: given a symbol name and file,
// finds all callers and references across the workspace.
//
// This is a simplified version that searches for the symbol name
// in all source files within the workspace. For full cross-file
// analysis with accurate AST matching, use the RefGraph from
// the indexer package.
func Analyze(ctx context.Context, parser *Parser, symbol, targetFile, workspaceRoot string) ([]Impact, error) {
	var impacts []Impact

	err := filepath.Walk(workspaceRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Skip non-source files and common ignored directories.
		base := filepath.Base(filepath.Dir(path))
		if base == ".git" || base == "node_modules" || base == "vendor" {
			return filepath.SkipDir
		}

		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		if !IsSupported(ext) {
			return nil
		}

		relPath, _ := filepath.Rel(workspaceRoot, path)

		// Skip the target file itself.
		if relPath == targetFile {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Parse and search for references.
		lang := ext
		if canonical, ok := langAliases[ext]; ok {
			lang = canonical
		}

		tree, err := parser.Parse(ctx, source, lang)
		if err != nil {
			return nil
		}

		// Walk the AST looking for identifiers matching the symbol name.
		WalkNodes(tree.Root, tree.language, func(node *gotreesitter.Node) bool {
			if node.Type(tree.language) == "identifier" || node.Type(tree.language) == "type_identifier" {
				text := nodeText(node, source)
				if text == symbol {
					// Find the containing function/method for context.
					contextStr := findContainingFunction(node, tree.language, source)

					impacts = append(impacts, Impact{
						Symbol:   symbol,
						File:     relPath,
						Line:     int(node.StartPoint().Row) + 1,
						Kind:     "references",
						Distance: 1,
						Context:  contextStr,
					})
				}
			}
			return true
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk workspace: %w", err)
	}

	return impacts, nil
}

// findContainingFunction walks up the AST to find the enclosing function.
func findContainingFunction(node *gotreesitter.Node, lang *gotreesitter.Language, source []byte) string {
	current := node.Parent()
	for current != nil {
		switch current.Type(lang) {
		case "function_declaration", "method_declaration", "function_definition",
			"function_item", "method":
			name := current.ChildByFieldName("name", lang)
			if name != nil {
				return fmt.Sprintf("in %s()", nodeText(name, source))
			}
			return "in anonymous function"
		}
		current = current.Parent()
	}
	return "at module level"
}
