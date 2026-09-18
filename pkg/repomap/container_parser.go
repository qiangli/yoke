package repomap

import (
	"context"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/treesitter"
)

// parseFilesWithTreeSitter parses non-Go source files using the in-process
// pure Go tree-sitter implementation. No container or CGO required.
func parseFilesWithTreeSitter(ctx context.Context, files []fileInfo) ([]Symbol, error) {
	if len(files) == 0 {
		return nil, nil
	}

	parser := treesitter.NewParser()
	var allSymbols []Symbol

	for _, f := range files {
		src, err := os.ReadFile(f.path)
		if err != nil {
			continue // skip unreadable files
		}

		tree, err := parser.Parse(ctx, src, f.lang)
		if err != nil {
			continue // skip unparseable files
		}

		tsSymbols := treesitter.ExtractSymbols(tree, f.rel)
		for _, ts := range tsSymbols {
			allSymbols = append(allSymbols, Symbol{
				Name:      ts.Name,
				Kind:      ts.Kind,
				Signature: ts.Signature,
				File:      ts.File,
				Line:      ts.Line,
				Exported:  ts.Exported,
			})
		}
	}

	return allSymbols, nil
}

// fileInfo holds metadata for a source file to parse.
type fileInfo struct {
	path string // absolute path on host
	rel  string // relative to repo root
	lang string // language name for tree-sitter
}

// langForExt returns the tree-sitter language name for a file extension.
func langForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".py":
		return "python"
	case ".js":
		return "javascript"
	case ".jsx":
		return "jsx"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	default:
		return ""
	}
}
