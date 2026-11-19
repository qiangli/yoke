package meet

import (
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
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
	again, err := RouteResponsibility(req)
	if err != nil || again.Escalation == nil || again.Escalation.Ref != receipt.Escalation.Ref {
		t.Fatalf("full retry receipt: %+v, %v", again, err)
	}
	events, err = readRoomTranscript(st.ID)
	if err != nil || len(events) != 5 {
		t.Fatalf("full retry wrote duplicate event: %d, %v", len(events), err)
	}
}

func TestRouteResponsibilityRetryNormalizesSeatAlias(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"accountable", "fallback"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	req := ResponsibilityRequest{Room: st.ID, OperationID: "alias-case", From: st.Human,
		Accountable: "@accountable", Question: "status?", EscalateTo: "fallback"}
	first, err := RouteResponsibility(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RouteResponsibility(req)
	if err != nil || first.Question.Ref != second.Question.Ref || first.Escalation.Ref != second.Escalation.Ref {
		t.Fatalf("alias retry = %+v, %+v, %v", first, second, err)
	}
}

func TestRouteResponsibilityRetryPreservesDeliveredDecision(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"accountable", "fallback"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	const question = "[route:delivered-case:question] Question for accountable seat: status?"
	if _, err := PostAs(st.ID, st.Human, "accountable", question); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(st.ID, Event{Speaker: st.Human, Kind: "route-status", To: st.Human,
		Text: routeStatusMarker("delivered-case", "question", 1), TS: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	receipt, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, OperationID: "delivered-case", From: st.Human,
		Accountable: "accountable", Question: "status?", EscalateTo: "fallback"})
	if err != nil || receipt.Question.State != "delivered" || receipt.Escalation != nil {
		t.Fatalf("delivered retry: %+v, %v", receipt, err)
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("retry wrote escalation: %d, %v", len(events), err)
	}
}

func TestExistingBusRouteKeepsDeliveredDecision(t *testing.T) {
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	const body = "[route:bus-retry:question] Question for accountable seat: status?"
	seq, err := bus.PostMessageSeq(bus.Post{From: "sender", To: "accountable", Topic: "responsibility", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	// The resolver is shared with bus.Send; use its registered test names.
	oldNames, oldResolve := bus.FleetNames, bus.FleetResolveName
	bus.FleetNames = func() []string { return []string{"accountable"} }
	bus.FleetResolveName = func(s string) string {
		if s == "accountable" {
			return s
		}
		return ""
	}
	t.Cleanup(func() { bus.FleetNames, bus.FleetResolveName = oldNames, oldResolve })
	if _, err := bus.PostMessageSeq(bus.Post{From: "sender", To: "sender", Topic: "responsibility-status",
		Body: routeStatusMarker("bus-retry", "question", int(seq))}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := existingBusRoute("sender", "accountable", body)
	if err != nil || !ok || got.State != "delivered" || got.Ref != "mb:"+strconv.FormatInt(seq, 10) {
		t.Fatalf("bus retry: %+v, %v, %v", got, ok, err)
	}
}

func TestRouteResponsibilityAllowsAuthenticatedInstanceOrganizer(t *testing.T) {
	st := newRoom(t)
	store := fleet.NewInstanceStore(t.TempDir())
	old := bus.InstanceStoreFn
	bus.InstanceStoreFn = func() *fleet.InstanceStore { return store }
	t.Cleanup(func() { bus.InstanceStoreFn = old })
	inst, err := store.Open(fleet.Family{Name: "route-test", Display: "Route test", Policy: fleet.PolicySingle,
		Bindings: []string{"codex:test"}}, fleet.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st.Participants = append(st.Participants, inst.MailAddress(), "accountable", "fallback")
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	oldClaim := bus.CurrentSessionClaim
	bus.CurrentSessionClaim = func(string) string { return "route-test-session" }
	t.Cleanup(func() { bus.CurrentSessionClaim = oldClaim })
	t.Setenv("BASHY_PRINCIPAL", inst.URN())
	receipt, err := RouteResponsibility(ResponsibilityRequest{Room: st.ID, OperationID: "instance-case",
		From: inst.UUID, Accountable: "accountable", Question: "status?", EscalateTo: "fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Question.Ref == "" {
		t.Fatalf("missing question ref: %+v", receipt)
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Speaker != inst.MailAddress() || events[0].Role != string(RoleParticipant) || events[0].Kind != "message" {
		t.Fatalf("instance attribution: %+v", events[0])
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
