package meet

import (
	"strings"
	"testing"
)

func TestMeetMessageLocators(t *testing.T) {
	st := &State{ID: "m1"}
	text := "see dir:/tmp/x/file.go please"
	events := []Event{
		{Round: 1, Speaker: "a", Kind: "message", Text: text},
		{Round: 1, Speaker: "b", Kind: "turn", Text: "plain words only"},
	}
	var b strings.Builder
	writeContributions(&b, st, events, "")
	out := b.String()
	if !strings.Contains(out, text) {
		t.Fatalf("body not preserved: %s", out)
	}
	if strings.Count(out, "locator:") != 1 {
		t.Fatalf("want exactly one locator line: %s", out)
	}
	if got := EventLocators(events[1]); got != nil {
		t.Fatalf("unexpected locators: %v", got)
	}
	if events[0].Text != text {
		t.Fatal("Event.Text mutated")
	}
}
