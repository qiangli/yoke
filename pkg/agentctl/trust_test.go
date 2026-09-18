package agentctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// THE DATA-LOSS BUG. Preseeding opencode's permissions used to be a blind
// os.WriteFile, which overwrote the project's opencode.json — provider settings
// and all. opencode reads its model ENDPOINTS from that file (Moonshot has
// separate international and China hosts, and the wrong one fails opaquely), so
// bashy would silently delete the very config the agent needed, and the agent
// would then die with an "Unexpected server error" pointing nowhere near the
// cause. It looked, for months, like opencode was simply broken.
func TestPreseedOpencodeMergesAndDoesNotDestroyProviderConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")

	existing := `{
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {
	    "moonshot": { "options": { "baseURL": "https://api.moonshot.ai/v1" } }
	  },
	  "mcp": { "ycode": { "enabled": true } },
	  "permission": { "bash": "ask" }
	}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ApplyTrustPreseed(dir, "opencode.json"); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}

	// The project's own settings SURVIVE. This is the whole test.
	prov, ok := doc["provider"].(map[string]any)
	if !ok {
		t.Fatal("provider config was destroyed — the agent has lost its model endpoints")
	}
	ms := prov["moonshot"].(map[string]any)["options"].(map[string]any)
	if ms["baseURL"] != "https://api.moonshot.ai/v1" {
		t.Errorf("the Moonshot endpoint was lost: %v", ms)
	}
	if doc["mcp"] == nil {
		t.Error("mcp config was destroyed")
	}
	if doc["$schema"] == nil {
		t.Error("$schema was destroyed")
	}

	perm := doc["permission"].(map[string]any)
	// An explicit choice by the project is NOT overridden.
	if perm["bash"] != "ask" {
		t.Errorf(`the project said bash:"ask"; preseeding must not overrule it, got %v`, perm["bash"])
	}
	// The keys it did not set are granted, which is the point of preseeding.
	if perm["edit"] != "allow" || perm["webfetch"] != "allow" {
		t.Errorf("preseed failed to grant the unset permissions: %v", perm)
	}
}

// With no file at all, preseeding writes one. That path always worked; keep it.
func TestPreseedOpencodeCreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := ApplyTrustPreseed(dir, "opencode.json"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["permission"].(map[string]any)["edit"] != "allow" {
		t.Errorf("permissions not granted: %v", doc)
	}
}

// A config we cannot parse is a config we must not replace. Refuse, and let
// opencode complain about its own file — that error at least points at the truth.
func TestPreseedOpencodeRefusesToClobberUnparseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyTrustPreseed(dir, "opencode.json"); err == nil {
		t.Fatal("an unparseable config must be refused, not overwritten")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{ this is not json" {
		t.Error("the file was modified despite the parse failure")
	}
}
