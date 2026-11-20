package seedfit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAreaSim writes the sim matrix with half its items moved to a second
// area, so a config can select areas.
func writeAreaSim(t *testing.T, area string) string {
	t.Helper()
	p := writeSim(t)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i, l := range strings.Split(string(data), "\n") {
		if i > 0 && (strings.Contains(l, "bench-s") || strings.Contains(l, "bench-t") || strings.Contains(l, "bench-u")) {
			l = strings.Replace(l, "coding-debug-test", area, 1)
		}
		lines = append(lines, l)
	}
	os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
	return p
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte(body), 0o644)
	return p
}

func TestConfigSelectsAreas(t *testing.T) {
	m := writeAreaSim(t, "browser-use")
	_, def, _ := runMain("--matrix", m, "--format", "tsv")
	if !strings.Contains(def, "# areas: coding-debug-test,tool-use-agentic;") || !strings.Contains(def, "area-not-selected=24") {
		t.Fatalf("default must keep the built-in areas and skip browser-use:\n%s", def)
	}
	c := writeConfig(t, "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {weight: 0.5, hands_on: true}\n  browser-use: {weight: 0.5}\nexclude: [math]\n")
	code, out, errs := runMain("--matrix", m, "--config", c, "--format", "tsv")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "# areas: browser-use,coding-debug-test;") || strings.Contains(out, "area-not-selected") {
		t.Fatalf("config areas not applied:\n%s", out)
	}
}

func TestConfigRejectsSilentDrops(t *testing.T) {
	m := writeAreaSim(t, "browser-use")
	for name, body := range map[string]string{
		"unlisted area":   "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {}\n",
		"area typo":       "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {}\n  browser-use: {}\n  browse-use: {}\n",
		"wrong schema":    "schema: v0\nareas:\n  coding-debug-test: {}\n",
		"unknown key":     "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {}\nweights: {}\n",
		"no areas":        "schema: bashy-seedfit-config-v1\n",
		"included+excl":   "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {}\n  browser-use: {}\nexclude: [browser-use]\n",
		"negative weight": "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {weight: -1}\n  browser-use: {}\n",
	} {
		if code, _, errs := runMain("--matrix", m, "--config", writeConfig(t, body)); code != 2 || errs == "" {
			t.Errorf("%s: exit %d, stderr %q; want 2 with a message", name, code, errs)
		}
	}
	ok := "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {}\nexclude: [browser-use]\n"
	if code, _, errs := runMain("--matrix", m, "--config", writeConfig(t, ok)); code != 0 {
		t.Errorf("explicit exclude must be accepted: exit %d %s", code, errs)
	}
}
