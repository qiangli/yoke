package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// A literal binding resolves without a vault; an unbound name is a real "no".
func TestBoundValueReadsTheHostBinding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	tpl := "# test\nCLOUDBOX_TOKEN=literal-session-token\nOTHER=x\n"
	if err := os.WriteFile(filepath.Join(dir, "bashy", "secrets.map"), []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := BoundValue("CLOUDBOX_TOKEN"); !ok || v != "literal-session-token" {
		t.Fatalf("BoundValue(CLOUDBOX_TOKEN) = %q, %v", v, ok)
	}
	if v, ok := BoundValue("NOT_BOUND"); ok {
		t.Fatalf("BoundValue(NOT_BOUND) = %q, want unbound", v)
	}
	if _, ok := BoundValue(" "); ok {
		t.Fatal("an empty name must not resolve")
	}
}
