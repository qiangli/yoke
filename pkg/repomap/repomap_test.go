package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGoFile(t *testing.T) {
	dir := t.TempDir()
	src := `package example

import "fmt"

// Config holds configuration.
type Config struct {
	Name string
	Port int
}

// Handler defines the handler interface.
type Handler interface {
	Handle(req *Request) error
}

// NewConfig creates a new Config.
func NewConfig(name string, port int) *Config {
	return &Config{Name: name, Port: port}
}

// String returns the string representation.
func (c *Config) String() string {
	return fmt.Sprintf("%s:%d", c.Name, c.Port)
}

func unexportedHelper() {}

const Version = "1.0"

var defaultPort = 8080
`

	path := filepath.Join(dir, "example.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	symbols := parseGoFile(path, "example.go")

	// Check we got the expected symbols.
	names := make(map[string]string)
	for _, s := range symbols {
		names[s.Name] = s.Kind
	}

	expected := map[string]string{
		"Config":           "type",
		"Handler":          "interface",
		"NewConfig":        "func",
		"String":           "method",
		"unexportedHelper": "func",
		"Version":          "const",
		"defaultPort":      "var",
	}

	for name, kind := range expected {
		got, ok := names[name]
		if !ok {
			t.Errorf("missing symbol %q", name)
			continue
		}
		if got != kind {
			t.Errorf("symbol %q: got kind %q, want %q", name, got, kind)
		}
	}

	// Check exported flags.
	for _, s := range symbols {
		switch s.Name {
		case "Config", "Handler", "NewConfig", "String", "Version":
			if !s.Exported {
				t.Errorf("symbol %q should be exported", s.Name)
			}
		case "unexportedHelper", "defaultPort":
			if s.Exported {
				t.Errorf("symbol %q should not be exported", s.Name)
			}
		}
	}

	// Check function signature formatting.
	for _, s := range symbols {
		if s.Name == "NewConfig" {
			if !strings.Contains(s.Signature, "NewConfig(") {
				t.Errorf("NewConfig signature missing params: %q", s.Signature)
			}
			if !strings.Contains(s.Signature, "*Config") {
				t.Errorf("NewConfig signature missing return type: %q", s.Signature)
			}
		}
		if s.Name == "String" {
			if !strings.Contains(s.Signature, "(*Config)") {
				t.Errorf("String method signature missing receiver: %q", s.Signature)
			}
		}
	}
}

func TestGenerate(t *testing.T) {
	dir := t.TempDir()

	// Create a mini Go project.
	files := map[string]string{
		"go.mod": "module testproject\n\ngo 1.22\n",
		"main.go": `package main

func main() {}

type App struct{}

func NewApp() *App { return &App{} }
`,
		"internal/handler.go": `package internal

type Handler interface {
	Handle() error
}

func Process(h Handler) error { return h.Handle() }
`,
		"vendor/lib.go": `package lib
func Ignored() {}
`,
	}

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	rm, err := Generate(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should have entries for main.go and internal/handler.go but NOT vendor/lib.go.
	foundMain := false
	foundHandler := false
	foundVendor := false

	for _, entry := range rm.Entries {
		switch {
		case entry.Path == "main.go":
			foundMain = true
		case strings.Contains(entry.Path, "handler.go"):
			foundHandler = true
		case strings.Contains(entry.Path, "vendor"):
			foundVendor = true
		}
	}

	if !foundMain {
		t.Error("expected main.go in repo map")
	}
	if !foundHandler {
		t.Error("expected internal/handler.go in repo map")
	}
	if foundVendor {
		t.Error("vendor/lib.go should be excluded from repo map")
	}

	// Test formatting produces output.
	formatted := rm.Format()
	if formatted == "" {
		t.Error("expected non-empty formatted output")
	}
	if !strings.Contains(formatted, "# Repository Map") {
		t.Error("formatted output should contain header")
	}
	if !strings.Contains(formatted, "main.go") {
		t.Error("formatted output should contain main.go")
	}
}

func TestScoreByRelevance(t *testing.T) {
	rm := &RepoMap{
		Entries: []FileEntry{
			{
				Path:    "auth/handler.go",
				Symbols: []Symbol{{Name: "AuthHandler", Exported: true}},
				Score:   1.0,
			},
			{
				Path:    "db/store.go",
				Symbols: []Symbol{{Name: "Store", Exported: true}},
				Score:   1.0,
			},
		},
	}

	scoreByRelevance(rm, "auth handler")

	// auth/handler.go should score higher.
	if rm.Entries[0].Score <= rm.Entries[1].Score {
		t.Errorf("auth/handler.go (score %.1f) should score higher than db/store.go (score %.1f)",
			rm.Entries[0].Score, rm.Entries[1].Score)
	}
}

func TestTruncateToTokenBudget(t *testing.T) {
	rm := &RepoMap{}
	// Create many entries.
	for i := range 100 {
		rm.Entries = append(rm.Entries, FileEntry{
			Path: strings.Repeat("a", 50),
			Symbols: []Symbol{
				{Signature: strings.Repeat("b", 100)},
				{Signature: strings.Repeat("c", 100)},
			},
		})
		_ = i
	}

	truncateToTokenBudget(rm, 500) // ~2000 chars budget

	if len(rm.Entries) >= 100 {
		t.Errorf("expected truncation, got %d entries", len(rm.Entries))
	}
}

func TestShouldExclude(t *testing.T) {
	tests := []struct {
		rel      string
		patterns []string
		expected bool
	}{
		{"vendor/lib.go", []string{"vendor/**"}, true},
		{"src/main.go", []string{"vendor/**"}, false},
		{"foo_test.go", []string{"**/*_test.go"}, true},
		{"internal/handler_test.go", []string{"**/*_test.go"}, true},
		{"internal/handler.go", []string{"**/*_test.go"}, false},
	}

	for _, tt := range tests {
		got := shouldExclude(tt.rel, tt.patterns)
		if got != tt.expected {
			t.Errorf("shouldExclude(%q, %v) = %v, want %v", tt.rel, tt.patterns, got, tt.expected)
		}
	}
}
