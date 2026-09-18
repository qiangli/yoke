package foreman

// The host-kb bridge for foreman-driven sessions: the check-before-task
// half of the kb loop is done FOR the agent by prepending the top host-kb
// matches for the session goal to the composed prompt. Computed once per
// session (the goal doesn't change) and empty when nothing matches — a
// missing kb never costs tokens or blocks a session.

import (
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/recall"
)

func (s *Session) kbPreamble() string {
	if s.kbNote != nil {
		return *s.kbNote
	}
	note := composeKBNote(s.state.Goal)
	s.kbNote = &note
	return note
}

func composeKBNote(goal string) string {
	if strings.TrimSpace(goal) == "" {
		return ""
	}
	res := recall.Context(recall.Query{
		Text: goal, Rings: []string{recall.RingRepo, recall.RingHost},
		Forms: []string{kb.FormNote, kb.FormPage}, Budget: recall.PreambleBudget,
		OS: runtime.GOOS,
	}, recall.ContextReaders()...)
	// Deliberately no diagnosis on a miss: this text is prepended to the prompt
	// and paid unconditionally, unlike weave's on-disk KB.md.
	return recall.RenderContext(res)
}
