package watch

import "time"

// AN EXPLICIT SOURCE NARROWS A RULE FILE, IT DOES NOT JOIN IT.
//
// `--preset sprint --source sprint-thread:472` is a request to watch ONE card.
// Adding that source beside the preset's own unfiltered sprint-thread source —
// which is what "the command line appends" meant — reports every card on the
// host to a watcher that named one, and the extra lines are indistinguishable
// from the ones that were asked for. A filter that silently widens the request
// it was given is the same failure as a filter that matches nothing: the answer
// is not the question.

// NarrowSources applies the command line's source selections to a rule file.
//
// Naming a source of a kind REPLACES the file's source(s) of that kind. Kinds
// the command line did not name are untouched, so `--source mb:x` still gets
// the sprint preset's thread source; with no selection at all the file behaves
// exactly as it shipped.
//
// Everything but the target stays the FILE's — the backfill window, the task
// class, the quiet window — because the command line said WHICH object to
// watch, not how to watch it. A selection that carries its own value (an
// explicit --backfill) wins over the file's. A kind the file does not declare
// is appended, so a selection adds a source a preset never had rather than
// being refused.
//
// The receiver is a value and the slice is rebuilt, so the caller's rule file
// (a cached preset, shared with the next caller) is never mutated.
func (f RuleFile) NarrowSources(sel []SourceSpec) RuleFile {
	if len(sel) == 0 {
		return f
	}
	byKind := map[string][]SourceSpec{}
	var kinds []string
	for _, s := range sel {
		if _, seen := byKind[s.Type]; !seen {
			kinds = append(kinds, s.Type)
		}
		byKind[s.Type] = append(byKind[s.Type], s)
	}

	out := make([]SourceSpec, 0, len(f.Sources)+len(sel))
	placed := map[string]bool{}
	for _, base := range f.Sources {
		chosen, replaced := byKind[base.Type]
		if !replaced {
			out = append(out, base)
			continue
		}
		if placed[base.Type] {
			// A second file source of a replaced kind: dropped with the first,
			// so "replace" means replace rather than "replace one of them".
			continue
		}
		placed[base.Type] = true
		// The narrowed sources take the place of the file's, so the poll order
		// a rule file declared is the order its watcher keeps.
		for _, s := range chosen {
			out = append(out, narrowOnto(base, s))
		}
	}
	for _, kind := range kinds {
		if placed[kind] {
			continue
		}
		placed[kind] = true
		out = append(out, byKind[kind]...)
	}
	f.Sources = out
	return f
}

// narrowOnto is the merge: the file's source with the selection's target.
func narrowOnto(base, sel SourceSpec) SourceSpec {
	out := base
	out.Target = sel.Target
	if sel.Backfill.D() > 0 {
		out.Backfill = sel.Backfill
	}
	if sel.Locator != "" {
		out.Locator = sel.Locator
	}
	return out
}

// Backfiller is implemented by a source that opens by reporting records
// ALREADY PRESENT in its store.
//
// It exists so an invocation can SAY SO. A watcher started mid-flight must be
// able to pick up a conversation in progress, but its first lines are then the
// recent past, which reads exactly like a burst of live activity — and an
// operator who mistakes backfill for news acts on something that already
// happened.
type Backfiller interface {
	BackfillWindow() time.Duration
}

// BackfillWindow is the widest window any of these sources will report from,
// and zero when none of them backfills at all.
func BackfillWindow(sources []Source) time.Duration {
	var widest time.Duration
	for _, s := range sources {
		b, ok := s.(Backfiller)
		if !ok {
			continue
		}
		if w := b.BackfillWindow(); w > widest {
			widest = w
		}
	}
	return widest
}
