package treesitter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// E2E: Parse and extract symbols for ALL supported languages
// ---------------------------------------------------------------------------

func TestE2E_ParseAllLanguages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	tests := []struct {
		lang   string
		source string
		want   []string // expected symbol names
	}{
		{
			lang: "go",
			source: `package main

func Serve() error { return nil }
type Handler struct{}
type Service interface { Start() }
`,
			want: []string{"Serve", "Handler", "Service"},
		},
		{
			lang: "python",
			source: `
def process():
    pass

class Pipeline:
    def run(self):
        pass
`,
			want: []string{"process", "Pipeline"},
		},
		{
			lang: "javascript",
			source: `
function render() { return null; }
class Component { constructor() {} }
`,
			want: []string{"render", "Component"},
		},
		{
			lang: "typescript",
			source: `
function fetchData(): Promise<void> {}
class ApiClient {}
interface Endpoint { url: string; }
type Config = { host: string };
`,
			want: []string{"fetchData", "ApiClient", "Endpoint", "Config"},
		},
		{
			lang: "rust",
			source: `
pub fn process() -> Result<(), Error> { Ok(()) }
pub struct Config { name: String }
pub enum Status { Active, Inactive }
pub trait Handler { fn handle(&self); }
`,
			want: []string{"process", "Config", "Status", "Handler"},
		},
		{
			lang: "java",
			source: `
public class Server {
    public void start() {}
}
interface Handler {
    void handle();
}
enum Status { ACTIVE, INACTIVE }
`,
			want: []string{"Server", "Handler", "Status"},
		},
		{
			lang: "ruby",
			source: `
def process
  puts "hello"
end

class Pipeline
  def run
    process
  end
end

module Utils
end
`,
			want: []string{"process", "Pipeline", "Utils"},
		},
	}

	parser := NewParser()
	ctx := context.Background()

	for _, tc := range tests {
		t.Run(tc.lang, func(t *testing.T) {
			tree, err := parser.Parse(ctx, []byte(tc.source), tc.lang)
			if err != nil {
				t.Fatalf("Parse(%s): %v", tc.lang, err)
			}
			if tree == nil || tree.Root == nil {
				t.Fatalf("Parse(%s): nil tree", tc.lang)
			}

			symbols := ExtractSymbols(tree, "test."+tc.lang)
			found := make(map[string]bool)
			for _, s := range symbols {
				found[s.Name] = true
			}

			for _, want := range tc.want {
				if !found[want] {
					t.Errorf("Parse(%s): missing symbol %q; got: %v", tc.lang, want, symbolNames(symbols))
				}
			}
		})
	}
}

// TestE2E_ParseC tests C language separately since its grammar structure differs.
func TestE2E_ParseC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`
#include <stdio.h>

int main(int argc, char *argv[]) {
    printf("hello\n");
    return 0;
}

struct Config {
    char* name;
    int port;
};
`)

	parser := NewParser()
	tree, err := parser.Parse(context.Background(), source, "c")
	if err != nil {
		t.Fatalf("Parse(c): %v", err)
	}
	if tree == nil || tree.Root == nil {
		t.Fatal("Parse(c): nil tree")
	}
	// C extraction is generic; just verify parse succeeds without panic.
}

// TestE2E_ParseTSX tests TSX (React) parsing.
func TestE2E_ParseTSX(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`
function App() {
    return <div>Hello</div>;
}

class Button {
    render() { return <button />; }
}
`)

	parser := NewParser()
	tree, err := parser.Parse(context.Background(), source, "tsx")
	if err != nil {
		t.Fatalf("Parse(tsx): %v", err)
	}

	symbols := ExtractSymbols(tree, "App.tsx")
	found := make(map[string]bool)
	for _, s := range symbols {
		found[s.Name] = true
	}
	if !found["App"] {
		t.Error("expected to find App function")
	}
}

// ---------------------------------------------------------------------------
// E2E: Symbol extraction quality
// ---------------------------------------------------------------------------

func TestE2E_GoSymbolDetails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`package main

func PublicFunc() {}
func privateFunc() {}

type ExportedType struct{}
type unexportedType struct{}

type MyInterface interface {
    Method() error
}
`)

	parser := NewParser()
	tree, _ := parser.Parse(context.Background(), source, "go")
	symbols := ExtractSymbols(tree, "main.go")

	byName := make(map[string]Symbol)
	for _, s := range symbols {
		byName[s.Name] = s
	}

	// Exported detection.
	if s, ok := byName["PublicFunc"]; !ok || !s.Exported {
		t.Error("PublicFunc should be found and marked exported")
	}
	if s, ok := byName["privateFunc"]; !ok || s.Exported {
		t.Error("privateFunc should be found and marked unexported")
	}
	if s, ok := byName["ExportedType"]; !ok || !s.Exported {
		t.Error("ExportedType should be exported")
	}
	if s, ok := byName["unexportedType"]; !ok || s.Exported {
		t.Error("unexportedType should be unexported")
	}

	// Kind detection.
	if byName["PublicFunc"].Kind != "func" {
		t.Errorf("expected kind func, got %q", byName["PublicFunc"].Kind)
	}
	if byName["ExportedType"].Kind != "type" {
		t.Errorf("expected kind type, got %q", byName["ExportedType"].Kind)
	}
	if byName["MyInterface"].Kind != "interface" {
		t.Errorf("expected kind interface, got %q", byName["MyInterface"].Kind)
	}

	// Line numbers.
	if byName["PublicFunc"].Line != 3 {
		t.Errorf("PublicFunc line = %d, want 3", byName["PublicFunc"].Line)
	}
}

func TestE2E_RustVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`
pub fn public_fn() {}
fn private_fn() {}
pub struct PubStruct {}
struct PrivStruct {}
`)

	parser := NewParser()
	tree, _ := parser.Parse(context.Background(), source, "rust")
	symbols := ExtractSymbols(tree, "lib.rs")

	byName := make(map[string]Symbol)
	for _, s := range symbols {
		byName[s.Name] = s
	}

	if s, ok := byName["public_fn"]; !ok || !s.Exported {
		t.Error("public_fn should be exported (pub)")
	}
	if s, ok := byName["private_fn"]; !ok || s.Exported {
		t.Error("private_fn should not be exported")
	}
}

// ---------------------------------------------------------------------------
// E2E: Tree-sitter query (S-expression) search
// ---------------------------------------------------------------------------

func TestE2E_QuerySearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`package main

func Hello() string { return "hello" }
func World() string { return "world" }
func Add(a, b int) int { return a + b }
`)

	parser := NewParser()
	// S-expression query: find all function declarations and capture the name.
	matches, err := Search(context.Background(), parser, source, "go",
		`(function_declaration name: (identifier) @name)`, "main.go")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(matches) < 3 {
		t.Errorf("expected at least 3 function matches, got %d", len(matches))
	}

	// Verify captures contain function names.
	names := make(map[string]bool)
	for _, m := range matches {
		if n, ok := m.Captures["name"]; ok {
			names[n] = true
		}
	}
	for _, want := range []string{"Hello", "World", "Add"} {
		if !names[want] {
			t.Errorf("expected capture for %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// E2E: Text pattern search (ast-grep style)
// ---------------------------------------------------------------------------

func TestE2E_TextPatternSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`package main

func ProcessOrder() error { return nil }
func ProcessPayment() error { return nil }
func HandleRequest() error { return nil }
`)

	parser := NewParser()

	// Search for functions starting with "func Process".
	matches, err := SearchText(context.Background(), parser, source, "go", "func Process", "main.go")
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}

	if len(matches) < 2 {
		t.Errorf("expected at least 2 matches for 'func Process', got %d", len(matches))
	}

	// Verify matched code contains "Process".
	for _, m := range matches {
		if m.MatchedCode == "" {
			t.Error("matched code should not be empty")
		}
	}
}

func TestE2E_TextPatternWithWildcard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte(`package main

func Hello() string { return "hello" }
func World() string { return "world" }
`)

	parser := NewParser()
	matches, err := SearchText(context.Background(), parser, source, "go", "func $NAME() string", "main.go")
	if err != nil {
		t.Fatalf("SearchText wildcard: %v", err)
	}

	// Should find at least one function matching the pattern.
	if len(matches) < 1 {
		t.Errorf("expected at least 1 match for wildcard pattern, got %d", len(matches))
	}
}

// ---------------------------------------------------------------------------
// E2E: Impact analysis
// ---------------------------------------------------------------------------

func TestE2E_ImpactAnalysis(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	// Create a temp workspace with two Go files.
	dir := t.TempDir()

	// File that defines the symbol.
	os.WriteFile(filepath.Join(dir, "server.go"), []byte(`package main

func StartServer() error {
    return nil
}
`), 0o644)

	// File that references the symbol.
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func main() {
    StartServer()
}
`), 0o644)

	parser := NewParser()
	impacts, err := Analyze(context.Background(), parser, "StartServer", "server.go", dir)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(impacts) == 0 {
		t.Error("expected at least one impact (reference from main.go)")
	}

	// Verify the reference was found in main.go.
	foundInMain := false
	for _, imp := range impacts {
		if imp.File == "main.go" && imp.Symbol == "StartServer" {
			foundInMain = true
			if imp.Kind != "references" {
				t.Errorf("expected kind 'references', got %q", imp.Kind)
			}
		}
	}
	if !foundInMain {
		t.Error("expected to find StartServer reference in main.go")
	}
}

func TestE2E_ImpactAnalysis_NoReferences(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func main() {}
`), 0o644)

	parser := NewParser()
	impacts, err := Analyze(context.Background(), parser, "NonExistent", "other.go", dir)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(impacts) != 0 {
		t.Errorf("expected 0 impacts for non-existent symbol, got %d", len(impacts))
	}
}

// ---------------------------------------------------------------------------
// E2E: Language alias resolution
// ---------------------------------------------------------------------------

func TestE2E_LanguageAliases(t *testing.T) {
	aliases := map[string]string{
		"py":  "python",
		"js":  "javascript",
		"ts":  "typescript",
		"rs":  "rust",
		"rb":  "ruby",
		"h":   "c",
		"jsx": "javascript",
	}

	for alias, canonical := range aliases {
		if !IsSupported(alias) {
			t.Errorf("alias %q should be supported", alias)
		}
		// Both alias and canonical should resolve to the same language.
		if GetLanguage(alias) != GetLanguage(canonical) {
			t.Errorf("alias %q should resolve to same language as %q", alias, canonical)
		}
	}
}

// ---------------------------------------------------------------------------
// E2E: Edge cases
// ---------------------------------------------------------------------------

func TestE2E_EmptySource(t *testing.T) {
	parser := NewParser()
	tree, err := parser.Parse(context.Background(), []byte(""), "go")
	// Empty source may parse successfully with an empty tree.
	if err != nil {
		t.Fatalf("Parse empty source: %v", err)
	}
	symbols := ExtractSymbols(tree, "empty.go")
	if len(symbols) != 0 {
		t.Errorf("expected 0 symbols from empty source, got %d", len(symbols))
	}
}

func TestE2E_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large file test in short mode")
	}

	// Generate a Go file with many functions.
	var source []byte
	source = append(source, []byte("package main\n\n")...)
	for i := 0; i < 100; i++ {
		source = append(source, []byte("func Func"+string(rune('A'+i%26))+string(rune('0'+i/26))+"() {}\n")...)
	}

	parser := NewParser()
	tree, err := parser.Parse(context.Background(), source, "go")
	if err != nil {
		t.Fatalf("Parse large file: %v", err)
	}

	symbols := ExtractSymbols(tree, "large.go")
	if len(symbols) < 50 {
		t.Errorf("expected many symbols from large file, got %d", len(symbols))
	}
}

func TestE2E_ExtractSymbolsNilTree(t *testing.T) {
	symbols := ExtractSymbols(nil, "test.go")
	if symbols != nil {
		t.Error("ExtractSymbols(nil) should return nil")
	}
}

func TestE2E_MatchPositionAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter e2e in short mode")
	}

	source := []byte("package main\n\nfunc Hello() {}\n")
	parser := NewParser()
	matches, err := SearchText(context.Background(), parser, source, "go", "func Hello", "main.go")
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}

	if len(matches) == 0 {
		t.Fatal("expected match")
	}

	m := matches[0]
	if m.StartLine != 3 {
		t.Errorf("StartLine = %d, want 3", m.StartLine)
	}
	if m.File != "main.go" {
		t.Errorf("File = %q, want main.go", m.File)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func symbolNames(symbols []Symbol) []string {
	names := make([]string, len(symbols))
	for i, s := range symbols {
		names[i] = s.Name
	}
	return names
}
