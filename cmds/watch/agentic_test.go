package watchcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/watch"
)

// isolatePresets points the local preset ring at an empty directory, so these
// tests see the embedded baseline and never whatever the host operator has
// installed.
func isolatePresets(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_WATCH_DIR", t.TempDir())
}

func TestAgenticFlagsDoNotDisturbClassicWatch(t *testing.T) {
	// The classic path must be reached when no agentic flag is given: output on
	// stdout, the classic -g exit, and no alert machinery anywhere.
	out, errOut, code := runTool(t, "-n", "0.01", "-t", "-g", "--", "echo $$")
	if code != 0 {
		t.Fatalf("classic watch = (%q, %d)", out, code)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 1 {
		t.Fatalf("classic stdout = %q, want the two snapshots -g compared", out)
	}
	if errOut != "" || strings.Contains(out, "fire") {
		t.Fatalf("agentic output leaked into the classic path: (%q, %q)", out, errOut)
	}
}

func TestAgenticUntilExitsZeroOnTheFirstAlert(t *testing.T) {
	isolatePresets(t)
	// Every snapshot is a new shell, so the pid changes and the line diff has
	// something to report on the second cycle.
	_, errOut, code := runTool(t, "-n", "0.01", "--until", "--match", "kind=added", "--locator", "@test-host", "--", "echo $$")
	if code != 0 {
		t.Fatalf("code = %d, want 0 (%q)", code, errOut)
	}
	if !strings.Contains(errOut, "match-1 fire match") || !strings.Contains(errOut, "@test-host") {
		t.Fatalf("console alert = %q", errOut)
	}
}

func TestAgenticTimeoutExitsOne(t *testing.T) {
	isolatePresets(t)
	// Stable output and a filter nothing matches: the awaited thing does not
	// happen, and that must not look like success.
	_, errOut, code := runTool(t, "-n", "0.01", "--until", "--timeout", "60ms",
		"--match", "kind=added,text=NEVER-MATCHES-THIS", "--", "echo stable")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (%q)", code, errOut)
	}
}

func TestAgenticJSONSinkWritesNDJSONToStdout(t *testing.T) {
	isolatePresets(t)
	out, _, code := runTool(t, "-n", "0.01", "--until", "--json", "--match", "kind=added", "--locator", "@test-host", "--", "echo $$")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	line := strings.TrimSpace(out)
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("stdout = %q, want exactly one NDJSON line", out)
	}
	var rec struct {
		SchemaVersion string `json:"schema_version"`
		Rule          string `json:"rule"`
		State         string `json:"state"`
		Locator       string `json:"locator"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("ndjson: %v (%q)", err, line)
	}
	if rec.SchemaVersion != watch.SchemaVersion || rec.State != watch.StateFire || rec.Locator != "@test-host" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestAgenticErrexitKeepsTheClassicCode(t *testing.T) {
	isolatePresets(t)
	_, _, code := runTool(t, "-n", "0.01", "-e", "--until", "--match", "kind=added", "--", "exit 7")
	if code != 8 {
		t.Fatalf("code = %d, want 8 (classic -e)", code)
	}
}

func TestAgenticUsageErrors(t *testing.T) {
	isolatePresets(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown selector", []string{"--match", "stage=done", "--", "echo x"}, "unknown selector"},
		{"not key=value", []string{"--match", "stage", "--", "echo x"}, "is not key=value"},
		{"nothing to watch", []string{"--until"}, "nothing to watch"},
		{"nothing to alarm on", []string{"--json", "--", "echo x"}, "nothing to alarm on"},
		{"unknown source", []string{"--source", "nope", "--match", "kind=added"}, "unknown source type"},
		// Same refusal with a preset: a selection the host cannot build must
		// not pass for a narrowing of the preset's sources.
		{"unknown source with a preset", []string{"--preset", "sprint", "--source", "nope"}, "unknown source type"},
		{"preset and rules", []string{"--preset", "sprint", "--rules", "x.json"}, "pass one"},
		{"bad regexp", []string{"--match", "text=(", "--", "echo x"}, "error parsing regexp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errOut, code := runTool(t, tc.args...)
			if code == 0 || !strings.Contains(errOut, tc.want) {
				t.Fatalf("(%q, %d), want an error naming %q", errOut, code, tc.want)
			}
		})
	}
}

func TestAgenticListPresets(t *testing.T) {
	isolatePresets(t)
	out, _, code := runTool(t, "--list-presets")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"sprint", "agent-reply", "idle", "local ring:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing %q missing %q", out, want)
		}
	}
}

func TestAgenticRulesFile(t *testing.T) {
	isolatePresets(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	body := `{
  "schema_version": "bashy-watch-rules-v1",
  "name": "local",
  "rules": [{"type": "match", "name": "anything-new", "when": {"kinds": ["added"]}}]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, errOut, code := runTool(t, "-n", "0.01", "--until", "--rules", path, "--locator", "@test-host", "--", "echo $$")
	if code != 0 || !strings.Contains(errOut, "anything-new fire match") {
		t.Fatalf("(%q, %d)", errOut, code)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version":"bashy-watch-rules-v99"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, errOut, code = runTool(t, "--rules", bad, "--", "echo x"); code == 0 || !strings.Contains(errOut, "schema") {
		t.Fatalf("(%q, %d), want a refused schema version", errOut, code)
	}
}

func TestParseCriteria(t *testing.T) {
	c, err := parseCriteria("source=mb,kind=added,field=stage,after=done,text=accept,attr.to=operator,locator=sprint:*,not")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Source != "mb" || c.Field != "stage" || c.After != "done" || c.Text != "accept" ||
		c.Locator != "sprint:*" || !c.Negate || len(c.Kinds) != 1 || c.Attrs["to"] != "operator" {
		t.Fatalf("criteria = %+v", c)
	}
	if c, err = parseCriteria("text=~stalled"); err != nil || c.Text != "stalled" {
		t.Fatalf("(%+v, %v): the =~ spelling must be accepted", c, err)
	}
}

// sourceNames is what a built plan can still be asked about: the KINDS it will
// poll, in order.
func sourceNames(plan watch.Plan) []string {
	out := make([]string, 0, len(plan.Sources))
	for _, s := range plan.Sources {
		out = append(out, s.Name())
	}
	return out
}

func TestAgenticExplicitSourceNarrowsThePreset(t *testing.T) {
	isolatePresets(t)
	rc, _, _ := newRunContext(t)
	cases := []struct {
		name    string
		sources []string
		want    []string
	}{
		{"no selection keeps the preset", nil, []string{"sprint-thread", "mb"}},
		// One sprint-thread source, not two: the preset's unfiltered one is
		// replaced rather than kept beside the explicit card.
		{"one card", []string{"sprint-thread:472"}, []string{"sprint-thread", "mb"}},
		{"two cards", []string{"sprint:472", "sprint-thread:480"}, []string{"sprint-thread", "sprint-thread", "mb"}},
		{"mb only narrows mb", []string{"mb:seat-x"}, []string{"sprint-thread", "mb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &agenticOptions{preset: "sprint", sources: tc.sources}
			plan, code := buildPlan(rc, watchOptions{}, o)
			if code >= 0 {
				t.Fatalf("buildPlan = %d", code)
			}
			if got := sourceNames(plan); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("sources = %v, want %v", got, tc.want)
			}
		})
	}
}

// A fresh watcher's first lines are BACKFILL — the recent past, not live
// activity. Unannounced, that reads exactly like a burst of new events.
func TestAgenticAnnouncesTheBackfillWindow(t *testing.T) {
	isolatePresets(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	_, errOut, code := runTool(t, "-n", "0.01", "--source", "sprint-thread:472", "--backfill", "2h",
		"--match", "kind=added", "--timeout", "40ms")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (%q)", code, errOut)
	}
	if !strings.Contains(errOut, "backfill: reporting entries younger than 2h0m0s") {
		t.Fatalf("stderr = %q, want the backfill window announced", errOut)
	}
}

func TestAgenticWithoutBackfillSaysNothing(t *testing.T) {
	isolatePresets(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	_, errOut, code := runTool(t, "-n", "0.01", "--source", "sprint-thread:472",
		"--match", "kind=added", "--timeout", "40ms")
	if code != 1 || strings.Contains(errOut, "backfill") {
		t.Fatalf("(%q, %d): a baseline-only watcher has no window to announce", errOut, code)
	}
}
