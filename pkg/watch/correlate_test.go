package watch

import (
	"testing"
	"time"
)

// mbLevels is the message-board ladder the story names: delivered (it is on the
// board), read (the recipient's cursor moved past it), replied (they posted
// back). Only the last one is an answer — a queued message is never an
// acknowledgment.
func mbLevels() []AckLevel {
	return []AckLevel{
		{Name: "delivered", Match: func(ev, trig Event) bool {
			return ev.Kind == KindAdded && ev.Attr("id") == trig.Attr("id")
		}},
		{Name: "read", Match: func(ev, trig Event) bool {
			return ev.Field == "read" && ev.Attr("post") == trig.Attr("id")
		}},
		{Name: "replied", Match: func(ev, trig Event) bool {
			return ev.Kind == KindAdded && ev.Attr("from") == trig.Attr("to") && ev.Attr("to") == trig.Attr("from")
		}},
	}
}

func replyRule() *CorrelationRule {
	return NewCorrelation(Correlation{
		RuleName: "agent-reply",
		Trigger:  func(ev Event) bool { return ev.Kind == KindAdded && ev.Attr("from") != "" && ev.Attr("to") != "" },
		Key:      func(ev Event) string { return ev.Attr("conv") },
		Levels:   mbLevels(),
		Within:   15 * time.Minute,
	})
}

func post(id, from, to string, at time.Time) Event {
	return Event{At: at, Source: "mb", Kind: KindAdded, Locator: "mb:" + id,
		Attrs: map[string]string{"id": id, "from": from, "to": to, "conv": "pair:a|b"}}
}

func readReceipt(postID string, at time.Time) Event {
	return Event{At: at, Source: "mb", Kind: KindChanged, Field: "read",
		Attrs: map[string]string{"post": postID, "conv": "pair:a|b"}}
}

func TestCorrelationRespondedCarriesLatencyAndLevel(t *testing.T) {
	r := replyRule()
	if got := r.On(post("m1", "a", "b", at0), at0); len(got) != 0 {
		t.Fatalf("sending is not news: %+v", got)
	}
	if got := r.On(readReceipt("m1", at0.Add(2*time.Minute)), at0.Add(2*time.Minute)); len(got) != 0 {
		t.Fatalf("a read receipt is progress, not an answer: %+v", got)
	}
	got := r.On(post("m2", "b", "a", at0.Add(5*time.Minute)), at0.Add(5*time.Minute))
	if len(got) != 1 || got[0].Reason != ReasonResponded || got[0].State != StateFire {
		t.Fatalf("alerts = %+v", got)
	}
	if got[0].Fields["level"] != "replied" || got[0].Fields["latency"] != "5m0s" {
		t.Fatalf("fields = %v", got[0].Fields)
	}
	// The conversation is settled: the reply must not reopen as a new request,
	// or two agents talking would alarm forever.
	if later := r.Tick(at0.Add(2 * time.Hour)); len(later) != 0 {
		t.Fatalf("alerts = %+v, want none after the answer", later)
	}
}

func TestCorrelationUnansweredNamesTheLevelReached(t *testing.T) {
	r := replyRule()
	r.On(post("m1", "a", "b", at0), at0)
	if got := r.Tick(at0.Add(14 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired before the deadline: %+v", got)
	}
	fire := r.Tick(at0.Add(15 * time.Minute))
	if len(fire) != 1 || fire[0].Reason != ReasonUnanswered {
		t.Fatalf("alerts = %+v", fire)
	}
	if fire[0].Fields["level"] != "delivered" || fire[0].Fields["waited"] != "15m0s" {
		t.Fatalf("fields = %v, want delivered-but-not-read", fire[0].Fields)
	}

	// Read and still silent is a DIFFERENT alert: "b never looked" and "b read
	// it and said nothing" call for different follow-ups.
	q := replyRule()
	q.On(post("m1", "a", "b", at0), at0)
	q.On(readReceipt("m1", at0.Add(time.Minute)), at0.Add(time.Minute))
	fire = q.Tick(at0.Add(15 * time.Minute))
	if len(fire) != 1 || fire[0].Fields["level"] != "read" {
		t.Fatalf("alerts = %+v, want level read", fire)
	}
}

func TestCorrelationLateAfterUnanswered(t *testing.T) {
	r := replyRule()
	r.On(post("m1", "a", "b", at0), at0)
	if fire := r.Tick(at0.Add(15 * time.Minute)); len(fire) != 1 {
		t.Fatalf("deadline alerts = %+v", fire)
	}
	late := r.On(post("m2", "b", "a", at0.Add(20*time.Minute)), at0.Add(20*time.Minute))
	if len(late) != 1 || late[0].Reason != ReasonLate || late[0].Fields["latency"] != "20m0s" {
		t.Fatalf("alerts = %+v, want a late answer with its latency", late)
	}
	if after := r.Tick(at0.Add(30 * time.Minute)); len(after) != 0 {
		t.Fatalf("alerts = %+v, want the pending closed by the late answer", after)
	}
}

func TestCorrelationTracksConversationsIndependently(t *testing.T) {
	r := NewCorrelation(Correlation{
		RuleName: "agent-reply",
		Trigger:  func(ev Event) bool { return ev.Kind == KindAdded },
		Key:      func(ev Event) string { return ev.Attr("conv") },
		Levels:   mbLevels(),
		Within:   10 * time.Minute,
	})
	one := Event{At: at0, Kind: KindAdded, Locator: "mb:m1", Attrs: map[string]string{"id": "m1", "from": "a", "to": "b", "conv": "topic:release"}}
	two := Event{At: at0, Kind: KindAdded, Locator: "mb:m2", Attrs: map[string]string{"id": "m2", "from": "a", "to": "c", "conv": "topic:deploy"}}
	r.On(one, at0)
	r.On(two, at0)
	reply := Event{At: at0.Add(time.Minute), Kind: KindAdded, Attrs: map[string]string{"id": "m3", "from": "b", "to": "a", "conv": "topic:release"}}
	if got := r.On(reply, at0.Add(time.Minute)); len(got) != 1 || got[0].Fields["level"] != "replied" {
		t.Fatalf("alerts = %+v", got)
	}
	fire := r.Tick(at0.Add(10 * time.Minute))
	if len(fire) != 1 || fire[0].Reason != ReasonUnanswered || fire[0].Locator != two.Locator {
		t.Fatalf("alerts = %+v, want only the other conversation overdue", fire)
	}
}
