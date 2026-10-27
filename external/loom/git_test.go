package loom

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedGitConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "app.ini")
	original := "[storage]\nSERVE_DIRECT = true\n[git]\nGC_ARGS = --aggressive\nPATH = git\n"
	if err := os.WriteFile(cfg, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context) (string, error) { return "C:\\cache with spaces\\mingit\\cmd\\git.exe", nil }
	if err := ensureGitConfig(context.Background(), cfg, resolve); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PATH = C:/cache with spaces/mingit/cmd/git.exe", "GC_ARGS = --aggressive", "SERVE_DIRECT = true"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
	if err := ensureGitConfig(context.Background(), cfg, resolve); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(cfg)
	if string(again) != string(b) {
		t.Fatal("not idempotent")
	}
	sentinel := errors.New("provision failed")
	if err := ensureGitConfig(context.Background(), cfg, func(context.Context) (string, error) { return "", sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
	again, _ = os.ReadFile(cfg)
	if string(again) != string(b) {
		t.Fatal("failed provisioning changed config")
	}
}
