// Package repomap generates structured overviews of code repositories.
//
// It scans a repository and extracts top-level symbols (types, functions,
// interfaces, methods) from source files using language-specific parsers.
// The result is a token-budgeted summary that can be injected into an LLM
// system prompt to give the model a structural understanding of the codebase.
//
// Currently supports: Go (via go/ast), Python, JavaScript/TypeScript,
// Rust, and Java (via regex-based extraction).
package repomap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxTokenBudget is the default maximum token budget for the repo map.
// At ~4 chars/token this is roughly 16K characters.
const MaxTokenBudget = 4096

// Symbol represents a top-level code symbol.
type Symbol struct {
	Name       string
	Kind       string // "func", "type", "interface", "method", "class", "const", "var"
	Signature  string // e.g., "func Foo(x int) error"
	File       string // relative path
	Line       int
	Exported   bool
	Receiver   string // for methods: the receiver type
	ParentType string // for methods nested under a type
}

// FileEntry holds all symbols from a single file.
type FileEntry struct {
	Path    string
	Symbols []Symbol
	Score   float64 // relevance score (higher = more relevant)
}

// RepoMap is a structured overview of a repository.
type RepoMap struct {
	Root    string
	Entries []FileEntry
}

// Options configures repo map generation.
type Options struct {
	// MaxTokens is the maximum token budget for the output (default: 4096).
	MaxTokens int
	// ExcludePatterns are glob patterns to exclude (e.g., "vendor/**", "**/*_test.go").
	ExcludePatterns []string
	// RelevanceQuery is an optional query to rank files by relevance.
	RelevanceQuery string
	// MaxFiles limits the number of files included (0 = no limit, uses token budget).
	MaxFiles int
	// ChatContextFiles are files currently in the conversation context.
	// These get a 50x ranking boost (Aider-inspired personalization).
	ChatContextFiles []string
	// ContainerEngine is unused but kept for API compatibility.
	// Tree-sitter parsing now uses the pure Go implementation directly.
	// Deprecated: this field is ignored.
	ContainerEngine any
}

// DefaultOptions returns sensible defaults for repo map generation.
func DefaultOptions() *Options {
	return &Options{
		MaxTokens: MaxTokenBudget,
		ExcludePatterns: []string{
			"vendor/**", "node_modules/**", ".git/**", "dist/**", "build/**",
			"**/*_test.go", "**/*.test.*", "**/*.spec.*",
			"**/*.min.js", "**/*.min.css",
			"**/testdata/**", "**/fixtures/**",
			"priorart/**", "external/**", "reference/**",
		},
	}
}

// GenerateForFiles builds a repo map from an explicit file list, skipping
// the directory walk. Useful when the caller already knows which files to
// inspect (foreign agents passing the conversation's open files, or tooling
// that wants a per-PR-changed-files map).
//
// Each path may be absolute or relative to root. Unknown languages are
// silently dropped. Returns an empty RepoMap (not nil) when no input file
// produced any symbols.
func GenerateForFiles(root string, files []string, opts *Options) (*RepoMap, error) {
	if opts == nil {
		opts = DefaultOptions()
	}
	rm := &RepoMap{Root: root}

	var goFiles []string
	var tsFiles []fileInfo
	for _, p := range files {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, p)
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		if !isSupportedSourceFile(abs) {
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			rel = abs
		}
		ext := strings.ToLower(filepath.Ext(abs))
		if ext == ".go" {
			goFiles = append(goFiles, abs)
			continue
		}
		lang := langForExt(ext)
		if lang == "" {
			continue
		}
		tsFiles = append(tsFiles, fileInfo{path: abs, rel: rel, lang: lang})
	}

	for _, path := range goFiles {
		rel, _ := filepath.Rel(root, path)
		symbols := parseGoFile(path, rel)
		if len(symbols) == 0 {
			continue
		}
		rm.Entries = append(rm.Entries, FileEntry{Path: rel, Symbols: symbols, Score: 1.0})
	}

	if len(tsFiles) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		tsSymbols, err := parseFilesWithTreeSitter(ctx, tsFiles)
		if err != nil {
			slog.Warn("tree-sitter parsing unavailable in GenerateForFiles", "error", err)
		} else {
			byFile := make(map[string][]Symbol)
			for _, sym := range tsSymbols {
				byFile[sym.File] = append(byFile[sym.File], sym)
			}
			for file, symbols := range byFile {
				rm.Entries = append(rm.Entries, FileEntry{Path: file, Symbols: symbols, Score: 1.0})
			}
		}
	}

	if opts.RelevanceQuery != "" {
		scoreByRelevanceGraph(rm, opts.RelevanceQuery, opts.ChatContextFiles)
	}
	sort.Slice(rm.Entries, func(i, j int) bool {
		if rm.Entries[i].Score != rm.Entries[j].Score {
			return rm.Entries[i].Score > rm.Entries[j].Score
		}
		return rm.Entries[i].Path < rm.Entries[j].Path
	})
	if opts.MaxTokens > 0 {
		truncateToTokenBudget(rm, opts.MaxTokens)
	}
	if opts.MaxFiles > 0 && len(rm.Entries) > opts.MaxFiles {
		rm.Entries = rm.Entries[:opts.MaxFiles]
	}
	return rm, nil
}

// Generate creates a repo map for the given root directory.
func Generate(root string, opts *Options) (*RepoMap, error) {
	if opts == nil {
		opts = DefaultOptions()
	}

	rm := &RepoMap{Root: root}

	// Walk the tree and collect source files, split by parser type.
	var goFiles []string
	var tsFiles []fileInfo // non-Go files for tree-sitter

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == "vendor" ||
				base == "__pycache__" || base == ".tox" || base == ".venv" {
				return filepath.SkipDir
			}
			// Also short-circuit user-supplied directory exclusions
			// ("priorart/**", "external/**", "testdata/**", ...) at
			// the dir level so the walker doesn't descend into multi-
			// gigabyte subtrees just to filter their files out later.
			// Without this, scanning a project with a vendored
			// reference checkout takes minutes per startup.
			rel, _ := filepath.Rel(root, path)
			if rel != "." && shouldExcludeDir(rel, opts.ExcludePatterns) {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		if shouldExclude(rel, opts.ExcludePatterns) {
			return nil
		}

		if !isSupportedSourceFile(path) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".go" {
			goFiles = append(goFiles, path)
		} else {
			lang := langForExt(ext)
			if lang != "" {
				tsFiles = append(tsFiles, fileInfo{path: path, rel: rel, lang: lang})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk repo: %w", err)
	}

	// Parse Go files natively via go/ast.
	for _, path := range goFiles {
		rel, _ := filepath.Rel(root, path)
		symbols := parseGoFile(path, rel)
		if len(symbols) == 0 {
			continue
		}
		rm.Entries = append(rm.Entries, FileEntry{
			Path:    rel,
			Symbols: symbols,
			Score:   1.0,
		})
	}

	// Parse non-Go files via in-process tree-sitter (pure Go, no CGO/container needed).
	if len(tsFiles) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		tsSymbols, err := parseFilesWithTreeSitter(ctx, tsFiles)
		if err != nil {
			// Tree-sitter failure is non-fatal — log and continue with Go-only map.
			slog.Warn("tree-sitter parsing unavailable, repo map will include Go files only", "error", err)
		} else {
			// Group symbols by file.
			byFile := make(map[string][]Symbol)
			for _, sym := range tsSymbols {
				byFile[sym.File] = append(byFile[sym.File], sym)
			}
			for file, symbols := range byFile {
				rm.Entries = append(rm.Entries, FileEntry{
					Path:    file,
					Symbols: symbols,
					Score:   1.0,
				})
			}
		}
	}

	// Apply relevance scoring if a query is provided.
	if opts.RelevanceQuery != "" {
		scoreByRelevanceGraph(rm, opts.RelevanceQuery, opts.ChatContextFiles)
	}

	// Sort by score descending, then by path.
	sort.Slice(rm.Entries, func(i, j int) bool {
		if rm.Entries[i].Score != rm.Entries[j].Score {
			return rm.Entries[i].Score > rm.Entries[j].Score
		}
		return rm.Entries[i].Path < rm.Entries[j].Path
	})

	// Truncate to token budget.
	if opts.MaxTokens > 0 {
		truncateToTokenBudget(rm, opts.MaxTokens)
	}
	if opts.MaxFiles > 0 && len(rm.Entries) > opts.MaxFiles {
		rm.Entries = rm.Entries[:opts.MaxFiles]
	}

	return rm, nil
}

// Format renders the repo map as a human-readable string for injection
// into system prompts.
func (rm *RepoMap) Format() string {
	if len(rm.Entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("# Repository Map\n")
	b.WriteString("Top-level symbols in this codebase:\n\n")

	for _, entry := range rm.Entries {
		b.WriteString(fmt.Sprintf("## %s\n", entry.Path))
		for _, sym := range entry.Symbols {
			if sym.Signature != "" {
				b.WriteString(fmt.Sprintf("  %s\n", sym.Signature))
			} else {
				b.WriteString(fmt.Sprintf("  %s %s\n", sym.Kind, sym.Name))
			}
		}
		b.WriteByte('\n')
	}

	return b.String()
}

// shouldExcludeDir returns true when a directory should be pruned from
// the walk entirely. Only `dir/**`-style patterns prune directories;
// file-shaped patterns like `**/*_test.go` don't, since dir paths
// can't match a filename glob anyway. Mirrors shouldExclude's
// matching logic for the `**`-suffix subset only.
func shouldExcludeDir(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// shouldExclude checks if a relative path matches any exclusion pattern.
func shouldExclude(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, rel); matched {
			return true
		}
		if strings.Contains(pattern, "**") {
			// "dir/**" → exclude anything under "dir/"
			if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
				if strings.HasPrefix(rel, prefix+"/") || rel == prefix {
					return true
				}
			}
			// "**/*.ext" → match the filename pattern against any file
			if suffix, ok := strings.CutPrefix(pattern, "**/"); ok {
				if matched, _ := filepath.Match(suffix, filepath.Base(rel)); matched {
					return true
				}
			}
		}
	}
	return false
}

// isSupportedSourceFile returns true for file types we can parse.
func isSupportedSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".java":
		return true
	}
	return false
}

// scoreByRelevance assigns relevance scores based on query matching.
func scoreByRelevance(rm *RepoMap, query string) {
	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)

	for i := range rm.Entries {
		entry := &rm.Entries[i]
		score := 0.0

		// Path scoring.
		pathLower := strings.ToLower(entry.Path)
		for _, word := range queryWords {
			if strings.Contains(pathLower, word) {
				score += 5.0
			}
		}

		// Symbol scoring.
		for _, sym := range entry.Symbols {
			nameLower := strings.ToLower(sym.Name)
			for _, word := range queryWords {
				if strings.Contains(nameLower, word) {
					score += 3.0
				}
			}
			if sym.Exported {
				score += 0.5 // prefer exported symbols
			}
		}

		// Boost files with more exported symbols.
		exported := 0
		for _, sym := range entry.Symbols {
			if sym.Exported {
				exported++
			}
		}
		score += float64(exported) * 0.2

		entry.Score = score
	}
}

// truncateToTokenBudget removes entries from the end until the estimated
// token count fits within the budget. Rough estimate: 4 chars per token.
func truncateToTokenBudget(rm *RepoMap, maxTokens int) {
	maxChars := maxTokens * 4

	totalChars := 0
	cutoff := len(rm.Entries)
	for i, entry := range rm.Entries {
		entryChars := len(entry.Path) + 4 // "## path\n"
		for _, sym := range entry.Symbols {
			if sym.Signature != "" {
				entryChars += len(sym.Signature) + 4
			} else {
				entryChars += len(sym.Kind) + len(sym.Name) + 6
			}
		}
		totalChars += entryChars
		if totalChars > maxChars {
			cutoff = i
			break
		}
	}
	rm.Entries = rm.Entries[:cutoff]
}
