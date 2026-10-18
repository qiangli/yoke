package fleet

import (
	"bytes"
	"fmt"
	"strings"
)

// SinkEscape reports whether args carry the canonical-output escape
// (--full or --view=canonical / --view canonical) and returns args without it.
func SinkEscape(args []string) (rest []string, canonical bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--full" || a == "--view=canonical":
			canonical = true
		case a == "--view" && i+1 < len(args) && args[i+1] == ViewCanonical:
			canonical = true
			i++
		default:
			rest = append(rest, a)
		}
	}
	return rest, canonical
}

// SinkInput describes one canonical output headed for an agent reader.
type SinkInput struct {
	Prefs     ReaderPrefs
	Sources   ReaderPrefsSources
	Canonical bool   // --full / --view=canonical escape
	Recover   string // command line that reproduces the output; used in the pointer
}

// SinkResult is the reduced output plus bookkeeping.
type SinkResult struct {
	Output    []byte
	Truncated bool
	Total     int
	Kept      int
	Pointer   string
}

// ApplyReaderSink applies the reader's output-reduction budget at the agent
// sink. The producer's bytes are never altered except by suffix truncation at a
// line boundary; a recovery pointer naming the escape and the budget source is
// appended whenever bytes were dropped. Canonical (or view=canonical) bypasses
// reduction entirely.
func ApplyReaderSink(canonical []byte, in SinkInput) SinkResult {
	total := len(canonical)
	budget := in.Prefs.BudgetBytes
	if in.Canonical || in.Prefs.View == ViewCanonical || budget <= 0 || int64(total) <= budget {
		return SinkResult{Output: canonical, Total: total, Kept: total}
	}
	cut := canonical[:budget]
	if i := bytes.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i+1]
	}
	src := in.Sources.BudgetBytes
	if src == "" {
		src = SourceDefault
	}
	cmd := strings.TrimSpace(in.Recover)
	if cmd == "" {
		cmd = "<command>"
	}
	ptr := fmt.Sprintf("[output reduced: kept %d of %d bytes (budget_bytes=%d from %s); full output: %s --full]\n",
		len(cut), total, budget, src, cmd)
	out := make([]byte, 0, len(cut)+len(ptr)+1)
	out = append(out, cut...)
	if len(cut) > 0 && cut[len(cut)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, ptr...)
	return SinkResult{Output: out, Truncated: true, Total: total, Kept: len(cut), Pointer: ptr}
}
