package treesitter

import (
	"context"
	"testing"
)

func TestParseGo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter test in short mode")
	}

	source := []byte(`package main

func Hello() string {
	return "hello"
}

type Config struct {
	Name string
}

type Runner interface {
	Run() error
}
`)

	parser := NewParser()
	tree, err := parser.Parse(context.Background(), source, "go")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tree == nil || tree.Root == nil {
		t.Fatal("expected non-nil tree")
	}

	symbols := ExtractSymbols(tree, "main.go")
	if len(symbols) == 0 {
		t.Fatal("expected symbols from Go source")
	}

	// Verify we found the function, type, and interface.
	found := make(map[string]string) // name -> kind
	for _, s := range symbols {
		found[s.Name] = s.Kind
	}

	if found["Hello"] != "func" {
		t.Errorf("expected func Hello, got %q", found["Hello"])
	}
	if found["Config"] != "type" {
		t.Errorf("expected type Config, got %q", found["Config"])
	}
	if found["Runner"] != "interface" {
		t.Errorf("expected interface Runner, got %q", found["Runner"])
	}
}

func TestParsePython(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter test in short mode")
	}

	source := []byte(`
def hello():
    return "hello"

class Config:
    def __init__(self):
        self.name = ""

def _private():
    pass
`)

	parser := NewParser()
	tree, err := parser.Parse(context.Background(), source, "python")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	symbols := ExtractSymbols(tree, "main.py")

	found := make(map[string]bool)
	for _, s := range symbols {
		found[s.Name] = true
		if s.Name == "hello" && !s.Exported {
			t.Error("expected hello to be exported")
		}
		if s.Name == "_private" && s.Exported {
			t.Error("expected _private to not be exported")
		}
	}

	if !found["hello"] {
		t.Error("expected to find function hello")
	}
	if !found["Config"] {
		t.Error("expected to find class Config")
	}
}

func TestParseRust(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter test in short mode")
	}

	source := []byte(`
pub fn hello() -> String {
    String::from("hello")
}

pub struct Config {
    name: String,
}

pub trait Runner {
    fn run(&self) -> Result<(), Error>;
}
`)

	parser := NewParser()
	tree, err := parser.Parse(context.Background(), source, "rust")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	symbols := ExtractSymbols(tree, "lib.rs")
	found := make(map[string]string)
	for _, s := range symbols {
		found[s.Name] = s.Kind
	}

	if found["hello"] != "func" {
		t.Errorf("expected func hello, got %q", found["hello"])
	}
	if found["Config"] != "type" {
		t.Errorf("expected type Config, got %q", found["Config"])
	}
	if found["Runner"] != "interface" {
		t.Errorf("expected interface Runner, got %q", found["Runner"])
	}
}

func TestParseUnsupported(t *testing.T) {
	parser := NewParser()
	_, err := parser.Parse(context.Background(), []byte("code"), "brainfuck")
	if err == nil {
		t.Error("expected error for unsupported language")
	}
}

func TestSupportedLanguages(t *testing.T) {
	langs := SupportedLanguages()
	if len(langs) < 7 {
		t.Errorf("expected at least 7 supported languages, got %d", len(langs))
	}
}

func TestIsSupported(t *testing.T) {
	tests := []struct {
		lang string
		want bool
	}{
		{"go", true},
		{"python", true},
		{"py", true},
		{"javascript", true},
		{"js", true},
		{"brainfuck", false},
	}

	for _, tc := range tests {
		if got := IsSupported(tc.lang); got != tc.want {
			t.Errorf("IsSupported(%q) = %v, want %v", tc.lang, got, tc.want)
		}
	}
}

func TestSearchText(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tree-sitter test in short mode")
	}

	source := []byte(`package main

func Hello() string {
	return "hello"
}

func World() string {
	return "world"
}
`)

	parser := NewParser()
	matches, err := SearchText(context.Background(), parser, source, "go", "func Hello", "main.go")
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}

	if len(matches) == 0 {
		t.Error("expected at least one match for 'func Hello'")
	}
}
