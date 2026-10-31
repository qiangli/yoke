package webconsole

import (
	"testing"

	"github.com/qiangli/yoke/pkg/src"
)

func TestHistorySummaryUsesSavedMessage(t *testing.T) {
	for _, message := range []string{
		"Show newest notes first — COVERED: newest notes appear at top",
		"Edit session, 2 wishes: 1. Show newest notes first; 2. Highlight unread notes",
	} {
		view := toHistoryView(src.VersionHistory{Entries: []src.VersionEntry{{Request: message, Revision: "rev"}}})
		if got := view.Versions[0].Summary; got != message {
			t.Errorf("summary = %q, want saved message %q", got, message)
		}
	}
	view := toHistoryView(src.VersionHistory{Entries: []src.VersionEntry{
		{Request: "restore to abc", Revision: "abc", Kind: "restore"},
		{Request: "Show newest notes first", Revision: "abc", Kind: "activate"},
	}})
	if got := view.Versions[0].Summary; got != "Show newest notes first" {
		t.Errorf("restored summary = %q, want saved wish", got)
	}
}
