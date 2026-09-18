package recommend

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/qiangli/coreutils/pkg/nudge"
	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Enabled mirrors the shared hint gate.
func Enabled() bool { return nudge.Enabled() }

// notFoundRe matches the common shells' not-found diagnostics and captures the
// offending path: GNU ("cat: X: No such file or directory", "cat: cannot access
// 'X': No such file or directory") and BSD, plus a bare "X: not found".
// Case-insensitive: GNU/BSD say "No such file or directory"; Go's os errors
// (surfaced by the pure-Go builtins) say "no such file or directory".
var notFoundRe = regexp.MustCompile(`(?i)(?:cannot (?:access|open|stat) |open )?'?([^\s':]+)'?: no such file or directory`)

// NotFoundTargets extracts the paths a command reported as missing, de-duped in
// first-seen order. Empty when the failure was something else.
func NotFoundTargets(stderr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range notFoundRe.FindAllStringSubmatch(stderr, -1) {
		p := m[1]
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

type noteLine struct {
	Schema     string   `json:"schema_version"`
	Kind       string   `json:"kind"` // "recommend"
	Missing    string   `json:"missing"`
	DidYouMean []string `json:"did_you_mean"`
	Note       string   `json:"note"`
	Off        string   `json:"off"`
}

// Note builds the recommendation appended to a command's stderr: "no <missing>;
// did you mean <recs>?" — surfaced, never substituted. Empty if no recs.
func Note(missing string, recs []Scored) string {
	if len(recs) == 0 {
		return ""
	}
	names := make([]string, len(recs))
	for i, r := range recs {
		names[i] = r.Name
	}
	human := "no `" + missing + "` here — did you mean: " + strings.Join(names, ", ") +
		"? (bashy knows the local files; the target does not exist)"
	if weavecli.IsAgentDriven() {
		b, _ := json.Marshal(noteLine{
			Schema: nudge.SchemaVersion, Kind: "recommend", Missing: missing,
			DidYouMean: names, Note: human, Off: "BASHY_HINTS=off",
		})
		return "\n" + string(b) + "\n"
	}
	return "\n─── bashy suggests ─── " + human + " (silence: BASHY_HINTS=off)\n"
}
