package recall

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
)

// EnvKnowledge switches knowledge injection for a launched agent.
//
//	off (default)  the agent is launched exactly as before
//	on             a recalled preamble is prepended to its prompt
//
// DEFAULT OFF IS DELIBERATE AND TEMPORARY. Injection is the treatment arm of an
// experiment that has not returned yet: the composition claim has no supporting
// measurement at either orchestrating seat
// (dhnt/docs/composition-validation-experiment.md §6d/§6e). Turning it on for
// every user before it is measured would (a) change every agent's behaviour on an
// unproven feature and (b) destroy the control arm — once injection is the
// default, there is no clean B to compare against.
//
// Flip the default only when the leaderboard shows a positive lift with
// non-overlapping intervals. Until then this env var IS the experiment.
const EnvKnowledge = "BASHY_KNOWLEDGE"

// Enabled reports whether knowledge injection is on for this process.
func Enabled() bool { return enabledIn(os.Getenv(EnvKnowledge)) }

func enabledIn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "1", "true", "yes":
		return true
	default:
		return false
	}
}

// PreambleBudget caps the injected text. Small on purpose: measured retrieval
// showed k=1 beating k=4, which matches ~4-chunk human working memory, and an
// agent whose context is half preamble has less room for the actual task.
const PreambleBudget = 700

// Preamble is the compatibility entry point for callers that supply their own
// readers. It delegates ranking, budgeting, and rendering to the same Context
// assembler used by `kb context`.
func Preamble(goal string, readers ...Reader) string {
	if !Enabled() || strings.TrimSpace(goal) == "" {
		return ""
	}
	return RenderContext(Context(Query{
		Text: goal, K: 2, Budget: PreambleBudget,
	}, readers...))
}

// ContextReaders opens the same default readers as `kb context`. Callers select
// rings and forms in Query; they never open, rank, or render stores themselves.
func ContextReaders() []Reader { return openContextRings() }

// ContextPageReaders opens the repo and host page rings for an injector. An
// explicit repo root is needed by weave because it assembles a new workspace
// while the conductor may be running elsewhere.
func ContextPageReaders(repoRoot string) []Reader {
	repoDir := filepath.Join(repoRoot, kb.RepoSub)
	hostDir := kb.DefaultDir()
	return []Reader{
		RepoRing{Store: kb.Open(repoDir), Path: repoDir},
		HostRing{Store: kb.Open(hostDir), Path: hostDir},
	}
}

// RenderContext renders the blocks selected by Context. Keeping this beside the
// injection seam lets chat, foreman, and weave consume the command's exact text
// shape without maintaining private renderers.
func RenderContext(res ContextResult) string {
	if res.Abstained || len(res.Blocks) == 0 {
		return ""
	}
	var b bytes.Buffer
	renderContext(&b, res)
	if res.Budget.Limit <= 0 || b.Len() <= res.Budget.Limit {
		return b.String()
	}
	// Injected text has a byte ceiling in addition to Context's estimated-token
	// ceiling. Shorten prose before citations so truncation never turns a
	// recalled claim into unattributed text.
	var bounded strings.Builder
	for _, block := range res.Blocks {
		prefix := fmt.Sprintf("- [%s/%s] ", block.Ring, block.Form)
		suffix := fmt.Sprintf("  (%s)\n", block.Ref)
		room := res.Budget.Limit - bounded.Len() - len(prefix) - len(suffix)
		if room <= 0 {
			continue
		}
		body := strings.ReplaceAll(block.Text, "\n", " ")
		if len(body) > room {
			body = bytePrefix(body, room)
		}
		bounded.WriteString(prefix)
		bounded.WriteString(body)
		bounded.WriteString(suffix)
	}
	return bounded.String()
}

func bytePrefix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := 0
	for i := range s {
		if i > limit {
			break
		}
		end = i
	}
	return strings.TrimSpace(s[:end])
}
