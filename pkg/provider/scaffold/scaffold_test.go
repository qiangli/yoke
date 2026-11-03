package scaffold_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/scaffold"
)

func TestScaffoldInitDeclarative(t *testing.T) {
	dir := t.TempDir()
	err := scaffold.Init(scaffold.Options{
		Dir:  dir,
		Form: provider.FormDeclarative,
		Name: "test-decl",
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if err := scaffold.Validate(dir); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	for _, rel := range []string{
		"manifest.yaml",
		"config.example.json",
		"fixtures/account.json",
		"fixtures/regions.json",
		"fixtures/machine.json",
		"fixtures/machines.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected file %s missing: %v", rel, err)
		}
	}
}

func TestScaffoldInitExecutable(t *testing.T) {
	dir := t.TempDir()
	err := scaffold.Init(scaffold.Options{
		Dir:  dir,
		Form: provider.FormExecutable,
		Name: "test-exec",
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if err := scaffold.Validate(dir); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	for _, rel := range []string{
		"manifest.yaml",
		"plugin.sh",
		"main.go",
		"config.example.json",
		"fixtures/account.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected file %s missing: %v", rel, err)
		}
	}
}

func TestScaffoldValidateRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	// No manifest
	if err := scaffold.Validate(dir); err == nil {
		t.Fatal("expected error on empty directory")
	}

	// Bad yaml
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("invalid : [ yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scaffold.Validate(dir); err == nil {
		t.Fatal("expected error on syntax error")
	}
}
