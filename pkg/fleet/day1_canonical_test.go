package fleet

// Sprint: #379 (checklist 4.18 Day-1: kit:/type: aliases parse, name:/kind: emit).
//
// v1.0.0 stays Day-1: a legacy `kit:`/`type:` tool document reads, and every
// write emits the canonical pair. Day-2 drops the aliases post-1.0.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every Day-1 spelling of a tool identity must parse to the same canonical
// Name/Kind — and where both spellings are present, the canonical one wins,
// so a half-migrated file cannot silently re-peg the entry.
func TestParseToolDay1Aliases(t *testing.T) {
	const cli = "\ncli:\n  binary: x\n  launch:\n    exec: x --model {model} {prompt}\n"
	for _, tc := range []struct {
		name, doc, wantName, wantKind string
	}{
		{"kit-only", "kit: legacy\ntype: cli" + cli, "legacy", "cli"},
		{"type-only", "name: legacy\ntype: cli" + cli, "legacy", "cli"},
		{"both-legacy", "kit: legacy\ntype: cli" + cli, "legacy", "cli"},
		{"canonical", "name: legacy\nkind: cli" + cli, "legacy", "cli"},
		{"canonical-wins-name", "name: kept\nkit: dropped\nkind: cli" + cli, "kept", "cli"},
		{"canonical-wins-kind", "name: legacy\nkind: cli\ntype: func" + cli, "legacy", "cli"},
		{"bare-fallback", "kind: cli" + cli, "from-filename", "cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTool("from-filename", []byte(tc.doc), nil)
			if err != nil {
				t.Fatalf("ParseTool: %v", err)
			}
			if got.Name != tc.wantName || got.Kind != tc.wantKind {
				t.Errorf("Name/Kind = %q/%q, want %q/%q",
					got.Name, got.Kind, tc.wantName, tc.wantKind)
			}
		})
	}
}

// A tool read through a legacy spelling must write back canonical: the first
// save rewrites the document, so catalogs converge without a migration.
func TestMarshalEmitsCanonicalKeys(t *testing.T) {
	legacy, err := ParseTool("legacy",
		[]byte("kit: legacy\ntype: cli\ncli:\n  binary: legacy\n  launch:\n    exec: legacy --model {model} {prompt}\n"), nil)
	if err != nil {
		t.Fatalf("ParseTool: %v", err)
	}
	out, err := Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var keys map[string]any
	if err := yaml.Unmarshal(out, &keys); err != nil {
		t.Fatalf("emitted YAML does not parse: %v\n%s", err, out)
	}
	for _, want := range []string{"name", "kind"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("emitted document lacks canonical key %q:\n%s", want, out)
		}
	}
	for _, dropped := range []string{"kit", "type"} {
		if _, ok := keys[dropped]; ok {
			t.Errorf("emitted document still carries legacy alias %q:\n%s", dropped, out)
		}
	}
	if keys["name"] != "legacy" || keys["kind"] != "cli" {
		t.Errorf("identity did not survive the round trip: %v", keys)
	}
}

// SaveTool persists the canonical spelling even when the Tool value came
// from a legacy document — the on-disk catalog converges on rewrite.
func TestSaveToolWritesCanonicalKeys(t *testing.T) {
	root := t.TempDir()
	c := New(WithRoot(root))
	legacy, err := ParseTool("legacy",
		[]byte("kit: legacy\ntype: cli\ncli:\n  binary: legacy\n  launch:\n    exec: legacy --model {model} {prompt}\n"), nil)
	if err != nil {
		t.Fatalf("ParseTool: %v", err)
	}
	if err := c.SaveTool(legacy); err != nil {
		t.Fatalf("SaveTool: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "tools", "legacy.yaml"))
	if err != nil {
		t.Fatalf("saved entry unreadable: %v", err)
	}
	var keys map[string]any
	if err := yaml.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("saved entry is not YAML: %v", err)
	}
	if _, ok := keys["name"]; !ok {
		t.Errorf("saved entry lacks canonical name:\n%s", raw)
	}
	for _, dropped := range []string{"kit", "type"} {
		if _, ok := keys[dropped]; ok {
			t.Errorf("saved entry carries legacy alias %q:\n%s", dropped, raw)
		}
	}
	if !strings.Contains(string(raw), "kind: cli") {
		t.Errorf("saved entry lost kind: cli:\n%s", raw)
	}
}
