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
	for _, ev := range []Event{
		{Speaker: st.Human, Kind: "note", Text: "run the gate", TS: fixedNow()},
		{Speaker: st.Human, Kind: "decision", Text: "owner assigned", TS: fixedNow()},
	} {
		if err := AppendEvent(st.ID, ev); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := RouteResponsibility(ResponsibilityRequest{
		Room: st.ID, OperationID: "route-one", From: st.Human, Accountable: "accountable",
		Question: "Who owns the gate?", Observers: []string{"observer", "OBSERVER", "accountable"},
		Digest:     []DigestItem{{Authority: "proposal", Source: "room:" + st.ID + "#1", Text: "run the gate"}, {Authority: "human-decision", Source: "room:" + st.ID + "#2", Text: "owner assigned"}},
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
			if !strings.Contains(text, "[proposal] room:"+st.ID+"#1") || !strings.Contains(text, "[human-decision] room:"+st.ID+"#2") || !strings.Contains(text, "no action requested") {
				t.Errorf("authority lost in digest: %q", text)
			}
		}
	}
}

func TestRouteResponsibilityRejectsUnsourcedDigestBeforePost(t *testing.T) {
	st := newRoom(t)
	_, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, OperationID: "bad-route", From: st.Human, Accountable: "accountable", Question: "status?", Observers: []string{"observer"}, Digest: []DigestItem{{Authority: "decision", Source: "room:7", Text: "yes"}}, EscalateTo: "fallback"})
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

func TestRouteResponsibilityRejectsAgentSpoofingHuman(t *testing.T) {
	st := newRoom(t)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/codex")
	_, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, OperationID: "spoof-test", From: st.Human,
		Accountable: "codex", Question: "answer?", EscalateTo: "steward"})
	if err == nil || !strings.Contains(err.Error(), "cannot author as") {
		t.Fatalf("spoof = %v", err)
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil || len(events) != 0 {
		t.Fatalf("spoof wrote events: %d, %v", len(events), err)
	}
}

func TestRouteResponsibilityRetryAfterPartialDelivery(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"accountable", "observer", "second", "fallback"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(st.ID, Event{Speaker: st.Human, Kind: "note", Text: "progress", TS: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	req := ResponsibilityRequest{Room: st.ID, OperationID: "retry-case", From: st.Human,
		Accountable: "accountable", Question: "status?", Observers: []string{"observer", "second"},
		Digest: []DigestItem{{Authority: "proposal", Source: "room:" + st.ID + "#1", Text: "progress"}}, EscalateTo: "fallback"}
	digest, err := renderResponsibilityDigest(st, req.Digest, "observer")
	if err != nil {
		t.Fatal(err)
	}
	for _, pre := range []struct{ to, body string }{
		{"accountable", "[route:retry-case:question] Question for accountable seat: status?"},
		{"observer", "[route:retry-case:digest:observer] " + digest},
	} {
		if _, err := PostAs(st.ID, st.Human, pre.to, pre.body); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := RouteResponsibility(req)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Question.Ref != "room:"+st.ID+"#2" || len(receipt.Observers) != 2 || receipt.Observers[0].Ref != "room:"+st.ID+"#3" {
		t.Fatalf("partial retry receipt: %+v", receipt)
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil || len(events) != 5 {
		t.Fatalf("retry wrote duplicate or missed event: %d, %v", len(events), err)
	}
}

func TestRouteResponsibilityFiltersDirectedDigestSource(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"accountable", "observer", "second", "fallback"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(st.ID, Event{Speaker: st.Human, Kind: "note", To: "observer", Text: "private detail", TS: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	receipt, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, OperationID: "privacy-case", From: st.Human,
		Accountable: "accountable", Question: "status?", Observers: []string{"observer", "second"},
		Digest: []DigestItem{{Authority: "unverified", Source: "room:" + st.ID + "#1", Text: "private detail"}}, EscalateTo: "fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Observers) != 1 || receipt.Observers[0].To != "observer" {
		t.Fatalf("private digest receipt: %+v", receipt)
	}
	directed, _, _, _, err := UnreadRecords(st.ID, "second", 0)
	if err != nil || len(directed) != 0 {
		t.Fatalf("second received private digest: %d, %v", len(directed), err)
	}
}

func TestRouteResponsibilityRejectsForgedAuthority(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"accountable", "observer", "fallback"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(st.ID, Event{Speaker: "codex", Kind: "note", Text: "approved", TS: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	for _, authority := range []string{"human-decision", "verified-result"} {
		_, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, OperationID: "forge-" + authority, From: st.Human,
			Accountable: "accountable", Question: "status?", Observers: []string{"observer"},
			Digest: []DigestItem{{Authority: authority, Source: "room:" + st.ID + "#1", Text: "approved"}}, EscalateTo: "fallback"})
		if err == nil {
			t.Errorf("%s was accepted", authority)
		}
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("forged authority wrote messages: %d, %v", len(events), err)
	}
}
