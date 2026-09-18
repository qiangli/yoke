// Package treesitter provides in-process AST parsing and structural
// code search using tree-sitter grammars. It supports multiple languages
// and provides pattern-based code matching.
//
// Uses the pure Go tree-sitter implementation (gotreesitter), which
// requires no CGO and works on all platforms including cross-compilation.
package treesitter

// Match represents a structural code match.
type Match struct {
	File        string            `json:"file"`
	StartLine   int               `json:"start_line"`
	EndLine     int               `json:"end_line"`
	StartCol    int               `json:"start_col"`
	EndCol      int               `json:"end_col"`
	MatchedCode string            `json:"matched_code"`
	Captures    map[string]string `json:"captures,omitempty"`
}

// Symbol represents a top-level code symbol extracted from an AST.
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`      // func, type, interface, method, class, const, var
	Signature string `json:"signature"` // human-readable signature
	File      string `json:"file"`      // relative path
	Line      int    `json:"line"`
	Exported  bool   `json:"exported"`
}

// Impact represents a dependency relationship found during impact analysis.
type Impact struct {
	Symbol   string `json:"symbol"`   // the affected symbol name
	File     string `json:"file"`     // file containing the affected symbol
	Line     int    `json:"line"`     // line number
	Kind     string `json:"kind"`     // "calls", "called_by", "references"
	Distance int    `json:"distance"` // hops from the target symbol
	Context  string `json:"context"`  // surrounding code context
}
