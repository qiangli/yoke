package fleet

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

// SinkPlan is the reader's output-reduction decision for one agent sink.
type SinkPlan struct {
	BudgetBytes int    // feeds reduce.Config.BudgetBytes; 0 selects the reducer default
	Canonical   bool   // reduction bypassed (redaction still applies at the sink)
	Source      string // layer that supplied the effective budget
}

// PlanSink derives the sink decision from resolved reader preferences. It
// never touches bytes: truncation, spill and the recovery pointer belong to
// pkg/reduce, so producers stay canonical and omitted bytes stay recoverable.
func PlanSink(prefs ReaderPrefs, sources ReaderPrefsSources, escape bool) SinkPlan {
	src := sources.BudgetBytes
	if src == "" {
		src = SourceDefault
	}
	b := prefs.BudgetBytes
	if b < 0 {
		b = 0
	}
	return SinkPlan{
		BudgetBytes: int(b),
		Canonical:   escape || prefs.View == ViewCanonical,
		Source:      src,
	}
}
