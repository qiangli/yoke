// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package lexicon

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// The managed block. Everything between the markers is GENERATED — the whole point
// is that it cannot go stale, because nobody maintains it by hand.
const (
	BeginMarker = "<!-- BEGIN bashy lexicon (generated — do not edit by hand) -->"
	EndMarker   = "<!-- END bashy lexicon -->"
)

// MaxAlwaysOn caps the always-on projection.
//
// This is not a style choice, it is a measured constraint: tool/term selection
// accuracy DEGRADES past roughly 15-20 items in active rotation, and near-synonyms
// are the top failure mode. More vocabulary in context does NOT mean better
// resolution. So the always-on tier is a SELECTION — the precedence rule, the
// highest-value terms, and the resolver — never a dump of the registry.
//
// The long tail is reached by LOOKUP, which is why the resolver command is the most
// important line in the block.
const MaxAlwaysOn = 18

// EmitAgentsMD renders the managed block for AGENTS.md / CLAUDE.md.
//
// AGENTS.md is the neutral always-on layer: read natively by Codex, Cursor, Aider,
// Copilot, Gemini, Zed, Devin (and by Claude Code via CLAUDE.md). It is the only
// place a rule reaches every tool without any of them agreeing on a config format.
func (s *Store) EmitAgentsMD(project string) string {
	var b strings.Builder
	fmt.Fprintln(&b, BeginMarker)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Project lexicon")
	fmt.Fprintln(&b)
	// The precedence rule. This one sentence does most of the work in the entire
	// feature; everything else is machinery to keep it true.
	fmt.Fprintln(&b, "**In this workspace the words below are bashy verbs and agent bindings — NOT their")
	fmt.Fprintln(&b, "English senses, and not vendors' products.** When you see one, it names a thing on")
	fmt.Fprintln(&b, "this machine. Resolve it; do not assume.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "    bashy lexicon resolve <term> --json     # what does this word mean HERE?")
	fmt.Fprintln(&b)
	// The canonical sentence, with its exact syntax.
	//
	// Cross-tool testing earned this line. Cold codex and opencode sessions BOTH
	// resolved the meaning perfectly from the block above — "claude" is a host
	// binding, not a vendor's product; "handoff" is the bashy verb — and BOTH then
	// guessed the invocation wrong, writing `handoff claude` instead of
	// `bashy handoff --to claude`. The block taught MEANING but not USAGE, so they
	// had to invent the flag. Teaching the sentence costs two lines and removes the
	// guess.
	fmt.Fprintln(&b, "The sentence this vocabulary exists for, and its exact form:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "    \"handoff this to codex\"   →   bashy handoff --to codex -m \"<why, for the successor>\"")
	fmt.Fprintln(&b, "    \"resume it\"                →   bashy resume")
	fmt.Fprintln(&b)

	verbs, bindings := s.split()
	if len(bindings) > 0 {
		fmt.Fprintln(&b, "**Agent tools** (host-specific — each names a CLI *with the model bound to it here*;")
		fmt.Fprintln(&b, "the SAME word denotes a different binding on another machine):")
		fmt.Fprintln(&b)
		for _, c := range bindings {
			fmt.Fprintf(&b, "- **%s** — %s\n", c.PrefLabel, oneLine(c.Definition))
		}
		fmt.Fprintln(&b)
	}
	if len(verbs) > 0 {
		fmt.Fprintln(&b, "**Verbs:**")
		fmt.Fprintln(&b)
		for _, c := range verbs {
			fmt.Fprintf(&b, "- **%s** — %s\n", strings.TrimPrefix(c.PrefLabel, "bashy "), oneLine(c.Definition))
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "This is a selection, not the whole vocabulary — the rest is reached by lookup, above.")
	fmt.Fprintln(&b, "In written artifacts a term may be marked `[[handoff]]` so it is unmistakable; in")
	fmt.Fprintln(&b, "conversation it is used plainly, like any jargon. The marker is optional emphasis,")
	fmt.Fprintln(&b, "never required syntax.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, EndMarker)
	return b.String()
}

// split selects what goes in the always-on tier.
//
// TOOLS, not bindings. This is the selection rule and it is easy to get backwards
// (the first version did): a human says **"codex"**, not "codex-gpt-5.5". The tool
// is the word spoken in the sentence this whole feature exists to serve — "handoff
// this to codex" — and it is the word most likely to be misread as a vendor's
// product. The binding is what that word DENOTES, and it is one lookup away.
//
// Listing all eight bindings would spend the entire always-on budget on names
// nobody says, and crowd out the ones everybody does. That is what "a selection,
// not a dump" means in practice.
func (s *Store) split() (verbs, bindings []Concept) {
	for _, c := range s.Concepts {
		switch c.Kind {
		case KindTool:
			bindings = append(bindings, c) // the spoken word
		case KindVerb:
			verbs = append(verbs, c)
		}
	}
	sort.Slice(verbs, func(i, j int) bool { return verbs[i].PrefLabel < verbs[j].PrefLabel })

	// Prefer the verbs a team actually says. These are the ones that appear in
	// "handoff this to codex"-shaped sentences — the orchestration vocabulary.
	priority := map[string]bool{
		"bashy handoff": true, "bashy resume": true, "bashy weave": true,
		"bashy gate": true, "bashy sprint": true, "bashy meet": true,
		"bashy invoke": true, "bashy kb": true, "bashy skill": true,
		"bashy dag": true, "bashy foreman": true, "bashy sdlc": true,
	}
	var top, rest []Concept
	for _, v := range verbs {
		if priority[v.PrefLabel] {
			top = append(top, v)
		} else {
			rest = append(rest, v)
		}
	}
	budget := MaxAlwaysOn - len(bindings)
	if budget < 0 {
		budget = 0
	}
	out := top
	if len(out) > budget {
		out = out[:budget]
	}
	_ = rest // the tail is reached by `lexicon resolve`, by design
	return out, bindings
}

var blockRe = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(BeginMarker) + `.*?` + regexp.QuoteMeta(EndMarker) + `\n?`)

// WriteInto splices the managed block into a file, replacing any previous one.
// Idempotent: running it twice produces the same file, which is what makes it safe
// to wire into a hook or a gate.
func WriteInto(path, block string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(b)
	if blockRe.MatchString(content) {
		content = blockRe.ReplaceAllString(content, block)
	} else {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		content += block
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 100 {
		s = s[:97] + "…"
	}
	if s == "" {
		s = "(no description)"
	}
	return s
}
