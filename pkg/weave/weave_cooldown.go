package weave

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Per-tool cooldown store. When a fleet agent CLI hits a usage/rate limit,
// weave records the tool on cooldown until its parsed reset time. The
// orchestrator queries `weave fleet` before assigning work and re-engages a
// throttled tool automatically once its cooldown expires.
//
// The store is a single JSON file at <queueDir>/tool_cooldowns.json mapping
// tool-name → available-at (RFC3339). It is shared by concurrent `weave
// start` wrappers, so every write is a read-modify-write under a dedicated
// flock (cooldown.lock — separate from queue.lock so a cooldown write never
// blocks a queue transition) plus a temp-file rename. A missing or garbled
// file is treated as "no cooldowns" — a cooldown record is best-effort and
// must never fail a run.

const weaveCooldownFile = "tool_cooldowns.json"

// toolCooldowns is the on-disk shape: tool name → RFC3339 available-at,
// plus (optionally) tool name → cause (quota-exhausted | cooling-down).
// Causes is additive: files written before it existed parse fine, and a
// record with no cause reads as plain cooling-down.
type toolCooldowns struct {
	Tools  map[string]string `json:"tools"`
	Causes map[string]string `json:"causes,omitempty"`
}

func weaveCooldownPath(queueDir string) string {
	return filepath.Join(queueDir, weaveCooldownFile)
}

// loadToolCooldowns reads the cooldown file, tolerating a missing or
// garbled file (returns an empty map, never an error for those cases).
func loadToolCooldowns(queueDir string) toolCooldowns {
	tc := toolCooldowns{Tools: map[string]string{}}
	b, err := os.ReadFile(weaveCooldownPath(queueDir))
	if err != nil {
		return tc
	}
	var parsed toolCooldowns
	if err := json.Unmarshal(b, &parsed); err != nil || parsed.Tools == nil {
		return tc
	}
	tc.Tools = parsed.Tools
	tc.Causes = parsed.Causes
	return tc
}

func saveToolCooldowns(queueDir string, tc toolCooldowns) error {
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return err
	}
	path := weaveCooldownPath(queueDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recordToolCooldown marks tool unavailable until `until`. A later reset
// for the same tool overwrites an earlier one (a re-throttle extends the
// cooldown). Concurrency-safe: read-modify-write under the cooldown lock.
func recordToolCooldown(queueDir, tool string, until time.Time) error {
	return recordToolCooldownCause(queueDir, tool, until, "")
}

// recordToolCooldownCause is recordToolCooldown with the WHY on record:
// cause is weaveCooldownQuota or weaveCooldownRate (empty clears any prior
// cause), so `weave fleet` can report QUOTA-EXHAUSTED distinctly from a
// transient rate limit.
func recordToolCooldownCause(queueDir, tool string, until time.Time, cause string) error {
	tool = normalizeToolName(tool)
	if tool == "" || tool == "." {
		return nil
	}
	return withWeaveCooldownLock(queueDir, func(tc *toolCooldowns) error {
		tc.Tools[tool] = until.UTC().Format(time.RFC3339)
		if tc.Causes == nil {
			tc.Causes = map[string]string{}
		}
		if cause == "" {
			delete(tc.Causes, tool)
		} else {
			tc.Causes[tool] = cause
		}
		return nil
	})
}

// toolAvailableAt returns the recorded available-at instant for tool and
// whether one is on record. A garbled timestamp is treated as no record.
func toolAvailableAt(queueDir, tool string) (time.Time, bool) {
	t, _, ok := toolCooldownStatus(queueDir, tool)
	return t, ok
}

// toolCooldownStatus returns the recorded available-at instant and cause for
// tool. Cause defaults to weaveCooldownRate for records written before causes
// existed. A garbled timestamp is treated as no record.
func toolCooldownStatus(queueDir, tool string) (time.Time, string, bool) {
	tool = normalizeToolName(tool)
	tc := loadToolCooldowns(queueDir)
	raw, ok := tc.Tools[tool]
	if !ok {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, "", false
	}
	cause := tc.Causes[tool]
	if cause == "" {
		cause = weaveCooldownRate
	}
	return t, cause, true
}

// availableTools filters fleet down to the tools that are not on cooldown
// as of now (a tool whose reset has passed counts as available). Order is
// preserved from the input fleet. Tools with no record are always included.
func availableTools(queueDir string, fleet []string, now time.Time) []string {
	tc := loadToolCooldowns(queueDir)
	out := make([]string, 0, len(fleet))
	for _, tool := range fleet {
		key := normalizeToolName(tool)
		raw, ok := tc.Tools[key]
		if !ok {
			out = append(out, tool)
			continue
		}
		until, err := time.Parse(time.RFC3339, raw)
		if err != nil || !until.After(now) {
			out = append(out, tool)
		}
	}
	return out
}

// normalizeToolName lowercases and basenames a tool label so a cooldown
// keyed by "codex" matches an "/usr/local/bin/codex" lookup.
func normalizeToolName(tool string) string {
	return filepath.Base(strings.ToLower(strings.TrimSpace(tool)))
}

// weaveThrottleToolFromSignal best-effort extracts the underlying tool name
// when the recorded `it.Tool` is the `bash` wrapper (weave invokes a fleet
// CLI as `bash -c '<tool> ...'`, so the basename is often "bash"). It scans
// the throttle log tail / signal for a known fleet tool mention.
//
// TODO: this is a heuristic, not authoritative. When the wrapper records the
// real argv it should pass the underlying tool through directly; until then
// a `bash` cooldown with no identifiable inner tool is keyed under "bash"
// (documented limitation — a bash-wide cooldown is coarse but safe: it just
// nudges the orchestrator to pick a different fleet member).
func weaveThrottleToolFromSignal(tool, logTail string) string {
	norm := normalizeToolName(tool)
	if norm != "bash" && norm != "sh" && norm != "" {
		return norm
	}
	hay := strings.ToLower(logTail)
	// Longest-first so "claude-code" wins over "claude" when both appear.
	known := []string{"claude-code", "opencode", "claude", "codex", "aider", "gemini", "agy"}
	for _, k := range known {
		if strings.Contains(hay, k) {
			return k
		}
	}
	if norm == "" {
		return "bash"
	}
	return norm
}

// weaveToolDisplayName infers the underlying fleet tool from the launch argv
// so `weave list` shows "codex"/"claude"/"opencode"/... instead of the "bash"
// wrapper. Tools are launched as `bash -c '<tool> ...'` (or `bash <tool>-launch.sh`),
// so the argv[0] basename is usually "bash"; this scans the whole argv for a
// known fleet name, falling back to the argv[0] basename. (Heuristic — an
// explicit label would be authoritative; see the TODO on
// weaveThrottleToolFromSignal.)
func weaveToolDisplayName(toolArgs []string) string {
	if len(toolArgs) == 0 {
		return ""
	}
	return weaveThrottleToolFromSignal(toolArgs[0], strings.Join(toolArgs, " "))
}
