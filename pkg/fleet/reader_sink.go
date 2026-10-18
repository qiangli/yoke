package fleet

import "strconv"

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

	// Effective profile the sink renders for this reader, each with its source
	// layer. Canonical replaces View with ViewCanonical; Help and Dialect are
	// reported unchanged because they select delivery form, not output bytes.
	View, ViewSource       string
	Help, HelpSource       string
	Dialect, DialectSource string
}

// Profile is the one-line, content-free description of the effective profile.
func (p SinkPlan) Profile() string {
	return "view=" + p.View + "(" + p.ViewSource + ") help=" + p.Help + "(" + p.HelpSource + ") dialect=" + p.Dialect + "(" + p.DialectSource + ") budget_bytes=" + strconv.Itoa(p.BudgetBytes) + "(" + p.Source + ")"
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
	canonical := escape || prefs.View == ViewCanonical
	view := prefs.View
	if canonical {
		view = ViewCanonical
	}
	or := func(s string) string {
		if s == "" {
			return SourceDefault
		}
		return s
	}
	viewSrc := or(sources.View)
	if escape {
		viewSrc = SourceExplicit
	}
	return SinkPlan{
		BudgetBytes:   int(b),
		Canonical:     canonical,
		Source:        src,
		View:          view,
		ViewSource:    viewSrc,
		Help:          prefs.Help,
		HelpSource:    or(sources.Help),
		Dialect:       prefs.Dialect,
		DialectSource: or(sources.Dialect),
	}
}
