package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGoFile_Generics(t *testing.T) {
	dir := t.TempDir()
	src := `package example

type Set[T comparable] struct {
	items map[T]bool
}

func NewSet[T comparable]() *Set[T] {
	return &Set[T]{items: make(map[T]bool)}
}

func (s *Set[T]) Add(item T) {
	s.items[item] = true
}
`
	path := filepath.Join(dir, "generics.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "generics.go")

	names := map[string]string{}
	for _, s := range symbols {
		names[s.Name] = s.Kind
	}

	if _, ok := names["Set"]; !ok {
		t.Error("missing generic type Set")
	}
	if _, ok := names["NewSet"]; !ok {
		t.Error("missing generic func NewSet")
	}
	if kind, ok := names["Add"]; !ok || kind != "method" {
		t.Errorf("missing or wrong kind for method Add: %s", kind)
	}
}

func TestParseGoFile_EmbeddedTypes(t *testing.T) {
	dir := t.TempDir()
	src := `package example

import "sync"

type SafeMap struct {
	sync.RWMutex
	data map[string]string
}
`
	path := filepath.Join(dir, "embedded.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "embedded.go")

	found := false
	for _, s := range symbols {
		if s.Name == "SafeMap" && s.Kind == "type" {
			found = true
			if !strings.Contains(s.Signature, "struct") {
				t.Errorf("SafeMap signature should mention struct: %q", s.Signature)
			}
		}
	}
	if !found {
		t.Error("missing SafeMap type")
	}
}

func TestParseGoFile_InitFunc(t *testing.T) {
	dir := t.TempDir()
	src := `package example

func init() {
	// init should be captured
}
`
	path := filepath.Join(dir, "init.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "init.go")

	found := false
	for _, s := range symbols {
		if s.Name == "init" {
			found = true
			if s.Exported {
				t.Error("init should not be exported")
			}
		}
	}
	if !found {
		t.Error("missing init function")
	}
}

func TestParseGoFile_Interface(t *testing.T) {
	dir := t.TempDir()
	src := `package example

type Reader interface {
	Read(p []byte) (n int, err error)
}

type Closer interface {
	Close() error
}

type ReadCloser interface {
	Reader
	Closer
}
`
	path := filepath.Join(dir, "iface.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "iface.go")

	interfaces := 0
	for _, s := range symbols {
		if s.Kind == "interface" {
			interfaces++
		}
	}
	if interfaces != 3 {
		t.Errorf("expected 3 interfaces, got %d", interfaces)
	}
}

func TestParseGoFile_MultiReturn(t *testing.T) {
	dir := t.TempDir()
	src := `package example

func Divide(a, b float64) (float64, error) {
	if b == 0 {
		return 0, nil
	}
	return a / b, nil
}
`
	path := filepath.Join(dir, "multi.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "multi.go")

	for _, s := range symbols {
		if s.Name == "Divide" {
			if !strings.Contains(s.Signature, "float64, error") {
				t.Errorf("expected multi-return in signature: %q", s.Signature)
			}
		}
	}
}

func TestParseGoFile_ConstBlock(t *testing.T) {
	dir := t.TempDir()
	src := `package example

const (
	StatusOK    = 200
	StatusError = 500
)

var (
	defaultName = "test"
	MaxRetries  = 3
)
`
	path := filepath.Join(dir, "const.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "const.go")

	consts := 0
	vars := 0
	for _, s := range symbols {
		switch s.Kind {
		case "const":
			consts++
		case "var":
			vars++
		}
	}
	if consts != 2 {
		t.Errorf("expected 2 consts, got %d", consts)
	}
	if vars != 2 {
		t.Errorf("expected 2 vars, got %d", vars)
	}
}

func TestParseGoFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.go")
	os.WriteFile(path, []byte("package empty\n"), 0644)

	symbols := parseGoFile(path, "empty.go")
	if len(symbols) != 0 {
		t.Errorf("expected no symbols in empty file, got %d", len(symbols))
	}
}

func TestParseGoFile_SyntaxError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.go")
	os.WriteFile(path, []byte("this is not valid go"), 0644)

	symbols := parseGoFile(path, "bad.go")
	if symbols != nil {
		t.Errorf("expected nil symbols for syntax error, got %d", len(symbols))
	}
}

func TestParseGoFile_NonExistent(t *testing.T) {
	symbols := parseGoFile("/nonexistent/file.go", "file.go")
	if symbols != nil {
		t.Error("expected nil symbols for nonexistent file")
	}
}

func TestIsExported(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"Foo", true},
		{"foo", false},
		{"", false},
		{"_private", false},
		{"X", true},
	}
	for _, tt := range tests {
		if got := isExported(tt.name); got != tt.expected {
			t.Errorf("isExported(%q) = %v, want %v", tt.name, got, tt.expected)
		}
	}
}

func TestFormatTypeExpr_Coverage(t *testing.T) {
	// Test via parsing a file with diverse types.
	dir := t.TempDir()
	src := `package example

func Diverse(
	m map[string][]int,
	ch chan error,
	fn func(int) bool,
	iface interface{},
	variadic ...string,
) {}
`
	path := filepath.Join(dir, "diverse.go")
	os.WriteFile(path, []byte(src), 0644)

	symbols := parseGoFile(path, "diverse.go")
	if len(symbols) == 0 {
		t.Fatal("expected at least one symbol")
	}

	sig := symbols[0].Signature
	// Verify various type formatters were exercised.
	if !strings.Contains(sig, "map[") {
		t.Errorf("signature missing map type: %q", sig)
	}
	if !strings.Contains(sig, "chan") {
		t.Errorf("signature missing chan type: %q", sig)
	}
	if !strings.Contains(sig, "func(") {
		t.Errorf("signature missing func type: %q", sig)
	}
	if !strings.Contains(sig, "...") {
		t.Errorf("signature missing variadic: %q", sig)
	}
}
