package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Send's own contract: "an unresolvable target writes NOTHING and fails with
// choices — a post to a name nobody answers was a receipt indistinguishable
// from a real delivery." That held for --to but NOT for a selector: the
// audience was resolved AFTER the durable append and its error was discarded,
// so `mb send --role reviewer` posted to the board and reported success while
// reaching nobody.
func TestSendAudienceWritesNothingWhenTheSelectorCannotResolve(t *testing.T) {
	isolate(t)

	prev := FleetSelect
	t.Cleanup(func() { FleetSelect = prev })
	FleetSelect = func(Audience) ([]string, error) {
		return nil, errors.New("unknown role \"reviewer\"")
	}

	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", Audience: &Audience{Role: "reviewer"}, Body: "x"})
	if err == nil {
		t.Fatal("Send succeeded on an unresolvable selector; a receipt nobody can act on is the defect")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("error should name the unresolvable selector, got %v", err)
	}
	if after := boardLen(t); after != before {
		t.Errorf("board grew from %d to %d — a failed send must write NOTHING", before, after)
	}
}

// The resolving case still posts once and reports the recipients it reached.
func TestSendAudienceStillPostsWhenTheSelectorResolves(t *testing.T) {
	isolate(t)

	prev := FleetSelect
	t.Cleanup(func() { FleetSelect = prev })
	FleetSelect = func(Audience) ([]string, error) { return []string{"zoe"}, nil }

	before := boardLen(t)
	res, err := Send(SendRequest{From: "tester", Audience: &Audience{Role: "conductor"}, Body: "peers only"})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if after := boardLen(t); after != before+1 {
		t.Fatalf("board grew from %d to %d, want exactly one append", before, after)
	}
	if len(res.Deliveries) != 1 || res.Deliveries[0].To != "zoe" {
		t.Fatalf("deliveries = %+v, want one to zoe", res.Deliveries)
	}
	if !strings.Contains(res.Label, "conductor") {
		t.Errorf("label = %q, should name the selector so the receipt is readable", res.Label)
	}
}

// Role participates in Empty, or a role-only selector would be treated as no
// selector at all and silently broadcast to everyone.
func TestRoleCountsAsASelector(t *testing.T) {
	if (Audience{Role: "conductor"}).Empty() {
		t.Fatal("a role-only Audience reports Empty, so it would fall through to a broadcast")
	}
}

func boardLen(t *testing.T) int {
	t.Helper()
	posts, err := Posts()
	if err != nil {
		t.Fatalf("read board: %v", err)
	}
	return len(posts)
}

func TestSendAudienceRequiresResolver(t *testing.T) {
	for _, aud := range []Audience{{Role: "conductor"}, {Band: 4}, {Tool: "ycode"}, {Provider: "p"}, {Family: "f"}, {Version: "v"}} {
		t.Run(aud.describe(), func(t *testing.T) {
			isolate(t)
			previous := FleetSelect
			t.Cleanup(func() { FleetSelect = previous })
			FleetSelect = nil
			_, err := Send(SendRequest{From: "tester", Audience: &aud, Body: "selector requires resolution"})
			if err == nil || !strings.Contains(err.Error(), "resolver") {
				t.Fatalf("missing resolver error = %v", err)
			}
			if got := boardLen(t); got != 0 {
				t.Fatalf("failed send appended %d posts", got)
			}
		})
	}
}

func TestSendAudienceRejectsMixedRoleAndBinding(t *testing.T) {
	for _, tc := range []struct {
		field string
		aud   Audience
	}{
		{"band", Audience{Role: "conductor", Band: 4}},
		{"tool", Audience{Role: "conductor", Tool: "ycode"}},
		{"provider", Audience{Role: "conductor", Provider: "p"}},
		{"family", Audience{Role: "conductor", Family: "f"}},
		{"version", Audience{Role: "conductor", Version: "v"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			isolate(t)
			previous := FleetSelect
			t.Cleanup(func() { FleetSelect = previous })
			FleetSelect = func(Audience) ([]string, error) {
				t.Error("invalid selector reached resolver")
				return []string{"zoe"}, nil
			}
			_, err := Send(SendRequest{From: "tester", Audience: &tc.aud, Body: "must refuse mixed selectors"})
			if err == nil || !strings.Contains(err.Error(), "role") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("mixed selector error = %v", err)
			}
			if got := boardLen(t); got != 0 {
				t.Fatalf("invalid selector appended %d posts", got)
			}
		})
	}
}

func TestSendAudienceZeroMatchesRecordsHonestReceipt(t *testing.T) {
	isolate(t)
	previous := FleetSelect
	t.Cleanup(func() { FleetSelect = previous })
	FleetSelect = func(Audience) ([]string, error) { return nil, nil }
	_, receipt, err := runMessageBoard(t, context.Background(), "send", "--role", "conductor", "record empty roster history")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt, "recorded") || !strings.Contains(receipt, "0 matching recipients") || strings.Contains(receipt, "posted to") {
		t.Fatalf("empty roster receipt = %q", receipt)
	}
	if got := boardLen(t); got != 1 {
		t.Fatalf("zero-match history has %d posts, want 1", got)
	}
}

func TestSendAudienceRejectsWhitespaceRole(t *testing.T) {
	isolate(t)
	previous := FleetSelect
	t.Cleanup(func() { FleetSelect = previous })
	FleetSelect = func(Audience) ([]string, error) {
		t.Error("whitespace role reached resolver")
		return []string{"unintended-recipient"}, nil
	}
	_, err := Send(SendRequest{From: "tester", Audience: &Audience{Role: " \t\n "}, Body: "must not broaden blank roles"})
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("whitespace role error = %v", err)
	}
	if got := boardLen(t); got != 0 {
		t.Fatalf("invalid role appended %d posts", got)
	}
}
