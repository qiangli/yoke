package meet

import (
	"strings"
	"testing"
)

func TestRouteResponsibilityDirectedRoomAndFallback(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"accountable", "observer", "unrelated", "fallback"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	receipt, err := RouteResponsibility(ResponsibilityRequest{
		Room: st.ID, From: st.Human, Accountable: "accountable",
		Question: "Who owns the gate?", Observers: []string{"observer", "OBSERVER", "accountable"},
		Digest:     []DigestItem{{Authority: "proposal", Source: "room:7", Text: "run the gate"}, {Authority: "human-decision", Source: "story:9", Text: "owner assigned"}},
		EscalateTo: "fallback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Observers) != 1 || receipt.Escalation == nil || receipt.Question.State == "delivered" {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, tc := range []struct {
		reader string
		want   int
	}{{"accountable", 1}, {"observer", 1}, {"fallback", 1}, {"unrelated", 0}} {
		directed, _, _, _, err := UnreadRecords(st.ID, tc.reader, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(directed) != tc.want {
			t.Errorf("%s received %d directed messages; want %d", tc.reader, len(directed), tc.want)
		}
		if tc.reader == "observer" && len(directed) == 1 {
			text := directed[0].Event.Text
			if !strings.Contains(text, "[proposal] room:7") || !strings.Contains(text, "[human-decision] story:9") || !strings.Contains(text, "no action requested") {
				t.Errorf("authority lost in digest: %q", text)
			}
		}
	}
}

func TestRouteResponsibilityRejectsUnsourcedDigestBeforePost(t *testing.T) {
	st := newRoom(t)
	_, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, From: st.Human, Accountable: "accountable", Question: "status?", Observers: []string{"observer"}, Digest: []DigestItem{{Authority: "decision", Source: "room:7", Text: "yes"}}, EscalateTo: "fallback"})
	if err == nil {
		t.Fatal("unrecognized authority was accepted")
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("invalid plan wrote %d messages", len(events))
	}
}
