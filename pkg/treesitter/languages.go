package treesitter

import (
	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// langLoaders maps language names to grammar constructor functions.
// Languages are loaded lazily on first use.
var langLoaders = map[string]func() *gotreesitter.Language{
	"go":         grammars.GoLanguage,
	"python":     grammars.PythonLanguage,
	"javascript": grammars.JavascriptLanguage,
	"typescript": grammars.TypescriptLanguage,
	"tsx":        grammars.TsxLanguage,
	"rust":       grammars.RustLanguage,
	"java":       grammars.JavaLanguage,
	"c":          grammars.CLanguage,
	"ruby":       grammars.RubyLanguage,
}

// langCache holds loaded languages to avoid repeated initialization.
var langCache = map[string]*gotreesitter.Language{}

// langAliases maps file extension aliases to canonical language names.
var langAliases = map[string]string{
	"go":   "go",
	"py":   "python",
	"js":   "javascript",
	"jsx":  "javascript",
	"ts":   "typescript",
	"tsx":  "tsx",
	"rs":   "rust",
	"java": "java",
	"c":    "c",
	"h":    "c",
	"rb":   "ruby",
}

// GetLanguage returns the tree-sitter language for the given name or alias.
// Returns nil if the language is not supported.
func GetLanguage(name string) *gotreesitter.Language {
	canonical := name
	if c, ok := langAliases[name]; ok {
		canonical = c
	}

	if lang, ok := langCache[canonical]; ok {
		return lang
	}

	loader, ok := langLoaders[canonical]
	if !ok {
		return nil
	}

	lang := loader()
	langCache[canonical] = lang
	return lang
}

// SupportedLanguages returns all supported language names.
func SupportedLanguages() []string {
	langs := make([]string, 0, len(langLoaders))
	for name := range langLoaders {
		langs = append(langs, name)
	}
	return langs
}

// IsSupported returns true if the given language name or alias is supported.
func IsSupported(name string) bool {
	return GetLanguage(name) != nil
}
