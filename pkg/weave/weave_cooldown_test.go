package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestParseThrottleReset(t *testing.T) {
	// Fixed reference: 2026-06-24 10:00:00 local.
	loc := time.Local
	now := time.Date(2026, 6, 24, 10, 0, 0, 0, loc)

	cases := []struct {
		name    string
		msg     string
		wantOK  bool
		wantStr string // RFC3339 in loc; empty when wantOK is false
	}{
		{
			name:    "codex exact message (AM, later today)",
			msg:     "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 11:41 AM.",
			wantOK:  true,
			wantStr: time.Date(2026, 6, 24, 11, 41, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "clock PM",
			msg:     "try again at 11:41 PM",
			wantOK:  true,
			wantStr: time.Date(2026, 6, 24, 23, 41, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "clock already past rolls to tomorrow",
			msg:     "try again at 9:00 AM",
			wantOK:  true,
			wantStr: time.Date(2026, 6, 25, 9, 0, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "clock 12 PM is noon",
			msg:     "back at 12:00 PM",
			wantOK:  true,
			wantStr: time.Date(2026, 6, 24, 12, 0, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "clock 12 AM is midnight (tomorrow, past)",
			msg:     "retry at 12:30 AM",
			wantOK:  true,
			wantStr: time.Date(2026, 6, 25, 0, 30, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "clock no minutes",
			msg:     "available again at 2 PM",
			wantOK:  true,
			wantStr: time.Date(2026, 6, 24, 14, 0, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "relative minutes",
			msg:     "rate limited, try again in 5 minutes",
			wantOK:  true,
			wantStr: now.Add(5 * time.Minute).Format(time.RFC3339),
		},
		{
			name:    "relative seconds",
			msg:     "too many requests; retry in 30 seconds",
			wantOK:  true,
			wantStr: now.Add(30 * time.Second).Format(time.RFC3339),
		},
		{
			name:    "relative hours singular",
			msg:     "quota exceeded, try again in 2 hours",
			wantOK:  true,
			wantStr: now.Add(2 * time.Hour).Format(time.RFC3339),
		},
		{
			name:    "relative abbreviated min",
			msg:     "wait in 15 min",
			wantOK:  true,
			wantStr: now.Add(15 * time.Minute).Format(time.RFC3339),
		},
		{
			name:    "retry-after header",
			msg:     "HTTP 429\nRetry-After: 120",
			wantOK:  true,
			wantStr: now.Add(120 * time.Second).Format(time.RFC3339),
		},
		{
			name:    "retry after no colon",
			msg:     "please retry after 60 seconds",
			wantOK:  true,
			wantStr: now.Add(60 * time.Second).Format(time.RFC3339),
		},
		{
			// The exact live codex phrasing observed 2026-07-21: a DATED reset
			// days out. The bare-clock parser could not see it, so no cooldown
			// was ever recorded and `weave fleet` kept saying "available".
			name:    "codex dated reset (month day ordinal, year)",
			msg:     "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Jul 24th, 2026 9:45 PM.",
			wantOK:  true,
			wantStr: time.Date(2026, 7, 24, 21, 45, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "dated reset without year, later this year",
			msg:     "try again at July 24 9:45 PM",
			wantOK:  true,
			wantStr: time.Date(2026, 7, 24, 21, 45, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:    "dated reset without year, already past rolls to next year",
			msg:     "try again at Jan 2nd 9:00 AM",
			wantOK:  true,
			wantStr: time.Date(2027, 1, 2, 9, 0, 0, 0, loc).Format(time.RFC3339),
		},
		{
			name:   "unparseable",
			msg:    "You've reached your weekly limit. Upgrade your plan.",
			wantOK: false,
		},
		{
			name:   "empty",
			msg:    "",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseThrottleReset(tc.msg, now)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (got=%v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if g := got.In(loc).Format(time.RFC3339); g != tc.wantStr {
				t.Fatalf("reset=%s want %s", g, tc.wantStr)
			}
		})
	}
}

func TestCooldownRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)

	if _, ok := toolAvailableAt(dir, "codex"); ok {
		t.Fatal("expected no cooldown on a fresh dir")
	}

	if err := recordToolCooldown(dir, "codex", until); err != nil {
		t.Fatalf("recordToolCooldown: %v", err)
	}
	got, ok := toolAvailableAt(dir, "codex")
	if !ok {
		t.Fatal("expected codex cooldown on record")
	}
	if !got.Equal(until) {
		t.Fatalf("cooldown=%v want %v", got, until)
	}

	// Path-form lookup normalizes to the same key.
	if _, ok := toolAvailableAt(dir, "/usr/local/bin/codex"); !ok {
		t.Fatal("expected path-form lookup to normalize to codex")
	}

	// Re-record extends/overwrites.
	later := now.Add(2 * time.Hour)
	if err := recordToolCooldown(dir, "codex", later); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	got, _ = toolAvailableAt(dir, "codex")
	if !got.Equal(later) {
		t.Fatalf("after re-record cooldown=%v want %v", got, later)
	}

	// A cause-less record (the pre-causes on-disk shape) reads as plain
	// cooling-down, never as available.
	if _, cause, ok := toolCooldownStatus(dir, "codex"); !ok || cause != weaveCooldownRate {
		t.Fatalf("cause-less record: ok=%v cause=%q, want ok with %q", ok, cause, weaveCooldownRate)
	}

	// A caused record carries its cause back out.
	if err := recordToolCooldownCause(dir, "codex", later, weaveCooldownQuota); err != nil {
		t.Fatalf("record with cause: %v", err)
	}
	if _, cause, _ := toolCooldownStatus(dir, "codex"); cause != weaveCooldownQuota {
		t.Fatalf("cause=%q want %q", cause, weaveCooldownQuota)
	}
}

func TestCooldownGarbledFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeTestFile(dir, weaveCooldownFile, "not json {{{"); err != nil {
		t.Fatal(err)
	}
	// Garbled file is treated as no cooldowns — not an error.
	if _, ok := toolAvailableAt(dir, "codex"); ok {
		t.Fatal("garbled file should read as no cooldown")
	}
	// A record still succeeds (overwrites the garbled file).
	if err := recordToolCooldown(dir, "codex", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("record over garbled file: %v", err)
	}
	if _, ok := toolAvailableAt(dir, "codex"); !ok {
		t.Fatal("expected cooldown after recording over garbled file")
	}
}

func TestAvailableTools(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	fleet := []string{"claude", "codex", "opencode", "aider"}

	// Nothing on cooldown: all available, order preserved.
	got := availableTools(dir, fleet, now)
	if len(got) != len(fleet) {
		t.Fatalf("avail=%v want all of %v", got, fleet)
	}
	for i := range fleet {
		if got[i] != fleet[i] {
			t.Fatalf("order not preserved: %v", got)
		}
	}

	// codex cooling until now+1h: filtered out at now.
	if err := recordToolCooldown(dir, "codex", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got = availableTools(dir, fleet, now)
	if contains(got, "codex") {
		t.Fatalf("codex should be filtered while cooling: %v", got)
	}
	if !contains(got, "claude") || !contains(got, "opencode") || !contains(got, "aider") {
		t.Fatalf("non-throttled tools should remain: %v", got)
	}

	// After the reset passes, codex is available again.
	got = availableTools(dir, fleet, now.Add(2*time.Hour))
	if !contains(got, "codex") {
		t.Fatalf("codex should re-appear after reset: %v", got)
	}
}

func TestThrottleToolFromSignal(t *testing.T) {
	pinFleet(t)
	cases := []struct {
		tool, log, want string
	}{
		{"codex", "anything", "codex"},
		{"/usr/bin/claude", "x", "claude"},
		{"bash", "ERROR: You've hit your usage limit ... (codex output)", "codex"},
		// "claude-code" is a spelling, not a tool: the signal canonicalizes
		// to the catalog name so the alias and the tool share one cooldown.
		{"bash", "claude-code: rate limited", "claude"},
		{"bash", "no recognizable tool here", "bash"},
		{"sh", "opencode hit a wall", "opencode"},
		// Newly recognized through the catalog (absent from the old frozen
		// list): muse and zcode, including the muse-code alias spelling.
		{"bash", "muse login expired", "muse"},
		{"bash", "muse-code crashed", "muse"},
		{"bash", "zcode --mode yolo failed", "zcode"},
	}
	for _, tc := range cases {
		if got := weaveThrottleToolFromSignal(tc.tool, tc.log); got != tc.want {
			t.Errorf("weaveThrottleToolFromSignal(%q, %q)=%q want %q", tc.tool, tc.log, got, tc.want)
		}
	}
}

// The signal table is the catalog's tool names plus aliases, longest-first.
// Every spelling resolves to a catalog tool, so a renamed or newly seeded
// tool is guessed without a Go change.
func TestWeaveKnownToolSpellingsFromCatalog(t *testing.T) {
	pinFleet(t)
	got := weaveKnownToolSpellings()
	if len(got) == 0 {
		t.Fatal("no known tool spellings from a seeded catalog")
	}
	for i := 1; i < len(got); i++ {
		if len(got[i].spelling) > len(got[i-1].spelling) {
			t.Fatalf("not longest-first at %d: %q before %q", i, got[i-1].spelling, got[i].spelling)
		}
		if got[i].spelling == got[i-1].spelling {
			t.Fatalf("duplicate spelling %q", got[i].spelling)
		}
	}
	cat := fleetCatalog()
	bySpelling := map[string]string{}
	for _, k := range got {
		bySpelling[k.spelling] = k.canonical
		if _, ok := cat.Tool(k.canonical); !ok {
			t.Errorf("canonical %q (spelling %q) resolves to no catalog tool", k.canonical, k.spelling)
		}
	}
	// The S584 set: every name the old frozen list knew, plus the tools it
	// missed (muse, zcode), keyed canonically.
	for spelling, canonical := range map[string]string{
		"claude": "claude", "codex": "codex", "opencode": "opencode",
		"agy": "agy", "aider": "aider", "gemini": "gemini",
		"muse": "muse", "muse-code": "muse", "zcode": "zcode",
	} {
		if got, ok := bySpelling[spelling]; !ok || got != canonical {
			t.Errorf("spelling %q -> %q, want %q", spelling, got, canonical)
		}
	}
}

// With no tools in the catalog (seeds off after the Mechanism→seed move),
// the frozen table applies unchanged.
func TestWeaveKnownToolSpellingsSeedsOffFallback(t *testing.T) {
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog {
		return fleet.New(fleet.WithRoot(t.TempDir()),
			fleet.WithoutCloudOverlay(), fleet.WithBaselineFS(fstest.MapFS{}))
	}
	t.Cleanup(func() { fleetCatalog = prev })
	got := weaveKnownToolSpellings()
	if len(got) != len(weaveKnownFallback) {
		t.Fatalf("fallback spellings = %d, want %d (%v)", len(got), len(weaveKnownFallback), got)
	}
	for i, k := range got {
		if k.spelling != weaveKnownFallback[i] || k.canonical != weaveKnownFallback[i] {
			t.Fatalf("fallback[%d] = %+v, want %q", i, k, weaveKnownFallback[i])
		}
	}
	if got := weaveThrottleToolFromSignal("bash", "claude-code: rate limited"); got != "claude-code" {
		t.Fatalf("fallback signal = %q, want claude-code", got)
	}
}

// The S584 byte-identical gate: every tool named by this migration runs its
// native default model/subscription launch, and that launch must render
// exactly what it rendered before the Go branches were removed. A tool with
// no bound model (or bound to its native seed model) renders its template
// with the model flag dropped — the native path no provider binding rewrites.
func TestNativeDefaultLaunchUnchanged(t *testing.T) {
	t.Setenv("BASHY_TOOLS_PATH", "")
	t.Setenv("BASHY_ROSTERS_PATH", "")
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	argv := map[string][]string{
		"claude":   {"claude", "--dangerously-skip-permissions", "-p", "THE PROMPT"},
		"codex":    {"codex", "exec", "--skip-git-repo-check", "--sandbox", "workspace-write", "THE PROMPT"},
		"opencode": {"opencode", "run", "--auto", "THE PROMPT"},
		"agy":      {"agy", "--dangerously-skip-permissions", "--print-timeout", "40m", "-p", "THE PROMPT"},
		"muse":     {"muse", "exec", "--yolo", "THE PROMPT"},
		"zcode":    {"zcode", "--mode", "yolo", "-p", "THE PROMPT"},
		"aider":    {"aider", "--yes-always", "--no-git", "--message", "THE PROMPT"},
		// gemini is detection-only: recognized, never driven, so no argv.
		"gemini": {},
	}
	for _, name := range []string{"claude", "codex", "opencode", "agy", "muse", "zcode", "aider", "gemini"} {
		tool, ok := cat.Tool(name)
		if !ok {
			t.Fatalf("no baseline tool %q", name)
		}
		if got := tool.Argv("", "THE PROMPT"); strings.Join(got, "\x00") != strings.Join(argv[name], "\x00") {
			t.Errorf("%s native argv =\n  %q\nwant\n  %q", name, got, argv[name])
		}
		if got := tool.LaunchEnv(fleet.LaunchVars{}); len(got) != 0 {
			t.Errorf("%s native env = %q, want empty (tool's own default survives)", name, got)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func writeTestFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

func TestWeaveToolDisplayName(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bash -c inline codex", []string{"bash", "-c", `codex exec --skip-git-repo-check "$WEAVE_ISSUE_BODY"`}, "codex"},
		{"bash -c inline claude", []string{"bash", "-c", `claude --dangerously-skip-permissions -p "$WEAVE_ISSUE_BODY"`}, "claude"},
		{"bash launcher with tool in path", []string{"bash", "/tmp/opencode-launch.sh"}, "opencode"},
		{"bash aider launcher", []string{"bash", "/tmp/aider-launch.sh"}, "aider"},
		{"direct codex", []string{"codex", "exec", "..."}, "codex"},
		{"direct claude path", []string{"/usr/local/bin/claude", "-p", "x"}, "claude"},
		{"unknown wrapper", []string{"bash", "-c", "mytool run"}, "bash"},
		{"empty", []string{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := weaveToolDisplayName(c.args); got != c.want {
				t.Fatalf("weaveToolDisplayName(%v) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}
