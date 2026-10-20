package watch

import "testing"

func TestCriteriaCompile(t *testing.T) {
	ev := Event{
		Source: "mb", Kind: KindChanged, Locator: "sprint:480", Key: "s1",
		Field: "status", Before: "open", After: "done",
		Text:  "story s1 accepted",
		Attrs: map[string]string{"to": "operator"},
	}
	cases := []struct {
		name string
		c    Criteria
		want bool
	}{
		{"field and after", Criteria{Field: "status", After: "done"}, true},
		{"after mismatch", Criteria{Field: "status", After: "failed"}, false},
		{"kind", Criteria{Kinds: []string{KindAdded, KindChanged}}, true},
		{"locator glob", Criteria{Locator: "sprint:*"}, true},
		{"locator glob miss", Criteria{Locator: "run:*"}, false},
		{"text regexp", Criteria{Text: "accept(ed)?"}, true},
		{"attr", Criteria{Attrs: map[string]string{"to": "operator"}}, true},
		{"attr miss", Criteria{Attrs: map[string]string{"to": "conductor"}}, false},
		{"source", Criteria{Source: "mb"}, true},
		{"any", Criteria{Any: []Criteria{{Source: "poll"}, {Source: "mb"}}}, true},
		{"all", Criteria{All: []Criteria{{Source: "mb"}, {Field: "status"}}}, true},
		{"negated", Criteria{Negate: true, Source: "poll"}, true},
		{"empty matches everything", Criteria{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.c.Compile()
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if got := p(ev); got != tc.want {
				t.Fatalf("predicate = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := (Criteria{Text: "("}).Compile(); err == nil {
		t.Fatal("a bad regexp must fail loudly, not match nothing")
	}
}

func TestMatchRuleFires(t *testing.T) {
	r, err := NewMatch("accepted", Criteria{Field: "stage", After: "accepted"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if r.Name() != "accepted" {
		t.Fatalf("name = %q", r.Name())
	}
	hit := r.On(Event{Kind: KindChanged, Locator: "sprint:480", Field: "stage", Before: "in", After: "accepted"}, at0)
	if len(hit) != 1 || hit[0].State != StateFire || hit[0].Reason != ReasonMatch || hit[0].Locator != "sprint:480" {
		t.Fatalf("alerts = %+v", hit)
	}
	if hit[0].Event == nil || hit[0].Message == "" {
		t.Fatalf("alert carries no evidence: %+v", hit[0])
	}
	if miss := r.On(Event{Kind: KindChanged, Field: "stage", After: "failed"}, at0); len(miss) != 0 {
		t.Fatalf("alerts = %+v, want none", miss)
	}
	if tick := r.Tick(at0); len(tick) != 0 {
		t.Fatalf("a match rule has nothing to say on a tick: %+v", tick)
	}
}

func TestChangeRuleSelectsFields(t *testing.T) {
	r, err := NewChange("stage-change", []string{"stage", "owner"}, Criteria{Locator: "sprint:*"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := r.On(Event{Kind: KindChanged, Locator: "sprint:480", Field: "stage", After: "in"}, at0); len(got) != 1 || got[0].Reason != ReasonChange {
		t.Fatalf("alerts = %+v", got)
	}
	if got := r.On(Event{Kind: KindChanged, Locator: "sprint:480", Field: "heat", After: "7"}, at0); len(got) != 0 {
		t.Fatalf("unselected field fired: %+v", got)
	}
	if got := r.On(Event{Kind: KindChanged, Locator: "run:repo-1", Field: "stage", After: "in"}, at0); len(got) != 0 {
		t.Fatalf("criteria ignored: %+v", got)
	}
}
