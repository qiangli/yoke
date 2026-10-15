package llmbudget

import "testing"

func TestTakeoverPolicy(t *testing.T) {
	p, err := New(Config{Policy: &Policy{Version: 1}}).TakeoverSettings()
	if err != nil || p.Mode != "handoff-only" || *p.MaxAutoTakeoversPerStory != 1 || !*p.PreferOtherVendorOnOutage {
		t.Fatalf("%+v %v", p, err)
	}
	n := 0
	b := false
	p, err = (TakeoverPolicy{Mode: "off", MaxAutoTakeoversPerStory: &n, PreferOtherVendorOnOutage: &b}).Effective()
	if err != nil || *p.MaxAutoTakeoversPerStory != 0 || *p.PreferOtherVendorOnOutage {
		t.Fatalf("%+v %v", p, err)
	}
	for _, mode := range []string{"off", "continue", "handoff-only"} {
		if _, err := (TakeoverPolicy{Mode: mode}).Effective(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (TakeoverPolicy{Mode: "bad"}).Effective(); err == nil {
		t.Fatal("accepted invalid mode")
	}
	n = -1
	if _, err := (TakeoverPolicy{MaxAutoTakeoversPerStory: &n}).Effective(); err == nil {
		t.Fatal("accepted negative limit")
	}
}
