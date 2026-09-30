package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// bandFilterCatalog seeds one L3 and one L5 model in an otherwise empty fleet,
// so an L5 filter has something to keep and something to drop.
func bandFilterCatalog(t *testing.T) []Option {
	t.Helper()
	opts := []Option{WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{})}
	c := New(opts...)
	if err := c.SaveModel(Model{Name: "lo3", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveModel(Model{Name: "hi5", Band: 5}); err != nil {
		t.Fatal(err)
	}
	return opts
}

func modelNames(t *testing.T, out string) []string {
	t.Helper()
	var rows []modelRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// Model list band filters keep exactly the L5 row, in both text and JSON.
func TestModelsListBandFilters(t *testing.T) {
	opts := bandFilterCatalog(t)

	out, err := runCmd(t, NewModelsCmd(opts...), "list", "--custom", "--json", "--band", "5")
	if err != nil {
		t.Fatalf("list --band 5: %v\n%s", err, out)
	}
	if names := modelNames(t, out); !has(names, "hi5") || has(names, "lo3") {
		t.Fatalf("--band 5 kept %v, want only hi5", names)
	}

	out, err = runCmd(t, NewModelsCmd(opts...), "list", "--custom", "--json", "--min-band", "5")
	if err != nil {
		t.Fatalf("list --min-band 5: %v\n%s", err, out)
	}
	if names := modelNames(t, out); !has(names, "hi5") || has(names, "lo3") {
		t.Fatalf("--min-band 5 kept %v, want only hi5", names)
	}

	out, err = runCmd(t, NewModelsCmd(opts...), "list", "--custom", "--json", "--min-band", "3")
	if err != nil {
		t.Fatalf("list --min-band 3: %v\n%s", err, out)
	}
	if names := modelNames(t, out); !has(names, "hi5") || !has(names, "lo3") {
		t.Fatalf("--min-band 3 kept %v, want hi5 and lo3", names)
	}

	text, err := runCmd(t, NewModelsCmd(opts...), "list", "--custom", "--band", "5")
	if err != nil {
		t.Fatalf("text list --band 5: %v\n%s", err, text)
	}
	if !strings.Contains(text, "hi5") || strings.Contains(text, "lo3") {
		t.Fatalf("text/JSON parity: text kept:\n%s", text)
	}
}

// The two band flags conflict, and nonzero values outside 1..MaxBand fail.
func TestModelsListBandRejectsConflictAndRange(t *testing.T) {
	opts := bandFilterCatalog(t)
	out, err := runCmd(t, NewModelsCmd(opts...), "list", "--custom", "--band", "5", "--min-band", "5")
	if err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Fatalf("conflict error = %v (%q), want alternatives", err, out)
	}
	for _, args := range [][]string{
		{"list", "--custom", "--band", "6"},
		{"list", "--custom", "--min-band", "9"},
		{"list", "--custom", "--band", "-1"},
	} {
		out, err := runCmd(t, NewModelsCmd(opts...), args...)
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("%v error = %v (%q), want out of range", args, err, out)
		}
	}
}

// Band help pegs the top at MaxBand (L5), not the stale L4 ceiling.
func TestBandHelpNamesMaxBand(t *testing.T) {
	agentList, _, err := NewAgentsCmd().Find([]string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"band", "min-band"} {
		use := agentList.Flags().Lookup(flag).Usage
		if !strings.Contains(use, "1-5") || strings.Contains(use, "1-4") {
			t.Fatalf("agent list --%s help = %q, want 1-5", flag, use)
		}
	}
	modelCmd := NewModelsCmd()
	for _, verb := range []string{"add", "set"} {
		sub, _, err := modelCmd.Find([]string{verb})
		if err != nil {
			t.Fatal(err)
		}
		use := sub.Flags().Lookup("band").Usage
		if !strings.Contains(use, "1-5") || strings.Contains(use, "1-4") {
			t.Fatalf("model %s --band help = %q, want 1-5", verb, use)
		}
	}
}
