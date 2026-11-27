// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package shape

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/qiangli/yoke/pkg/shape/state"
)

func autoLog(t *testing.T, s Service) []state.AutoDecision {
	t.Helper()
	log, err := state.ReadAutoLog(s.Options.Home)
	if err != nil {
		t.Fatal(err)
	}
	return log
}

func TestUnattendedWishRefusedWhenOff(t *testing.T) {
	s, c, from, _, _ := testService(t)
	if err := state.SetAuto(s.Options.Home, false); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"feed", "schedule"} {
		_, err := s.Import(context.Background(), c, Wish{Request: "change greeting", Source: Source{Kind: kind, Ref: "item-1"}}, from)
		if !errors.Is(err, ErrAutoOff) {
			t.Fatalf("%s: err = %v, want ErrAutoOff", kind, err)
		}
		if _, err = s.FileRequest(context.Background(), c, Wish{Request: "change greeting", Source: Source{Kind: kind, Ref: "item-1"}}); !errors.Is(err, ErrAutoOff) {
			t.Fatalf("%s request: err = %v, want ErrAutoOff", kind, err)
		}
	}
	if st, err := s.Status(context.Background(), c); err != nil || len(st.Candidates) != 0 {
		t.Fatalf("refused wish left candidates: %+v %v", st.Candidates, err)
	}
	log := autoLog(t, s)
	if len(log) != 5 { // the switch, then one refusal per wish
		t.Fatalf("log = %+v", log)
	}
	for i, d := range log[1:] {
		wantKind := []string{"feed", "feed", "schedule", "schedule"}[i]
		if d.Event != "refusal" || d.Result != "off" || d.Source != wantKind || d.Ref != "item-1" || d.App != "hello" {
			t.Fatalf("line %d = %+v", i+1, d)
		}
	}
	// The switch is for unattended sources only: a person's wish still runs.
	if _, err := s.Import(context.Background(), c, Wish{Request: "change greeting"}, from); err != nil {
		t.Fatal(err)
	}
}

func TestUnattendedWishLogsTriggerVerdictActivation(t *testing.T) {
	s, c, from, _, _ := testService(t)
	ctx := context.Background()
	src := Source{Kind: "feed", Ref: "abc123"}
	candidate, err := s.Import(ctx, c, Wish{Request: "change greeting", Source: src}, from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Verify(ctx, c, candidate.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Approve(ctx, c, candidate.ID)
	if err != nil || got.Status != Approved {
		t.Fatalf("%+v %v", got, err)
	}
	want := []struct{ event, result string }{
		{"trigger", "wish"},
		{"verdict", "pass"}, // shape verify
		{"verdict", "pass"}, // approve verifies again
		{"activation", "activated"},
	}
	log := autoLog(t, s)
	if len(log) != len(want) {
		t.Fatalf("log = %+v", log)
	}
	for i, w := range want {
		d := log[i]
		if d.Event != w.event || d.Result != w.result || d.Source != "feed" || d.Ref != "abc123" || d.App != "hello" || d.Detail == "" {
			t.Fatalf("line %d = %+v, want %+v", i, d, w)
		}
	}
}

func TestUnattendedWishLogsFailedVerdictAndNoActivation(t *testing.T) {
	s, c, from, v, a := testService(t)
	v.pass = false
	ctx := context.Background()
	candidate, err := s.Import(ctx, c, Wish{Request: "change greeting", Source: Source{Kind: "schedule", Ref: "nightly"}}, from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(ctx, c, candidate.ID); !errors.Is(err, ErrVerification) || a.calls != 0 {
		t.Fatalf("err = %v, activations = %d", err, a.calls)
	}
	log := autoLog(t, s)
	if len(log) != 3 || log[1].Event != "verdict" || log[1].Result != "fail" || log[2].Event != "activation" || log[2].Result != "failed" || log[2].Source != "schedule" {
		t.Fatalf("log = %+v", log)
	}
}

func TestHumanWishLogsNothing(t *testing.T) {
	for _, src := range []Source{{}, {Kind: "human", Ref: "alice"}} {
		s, c, from, _, _ := testService(t)
		ctx := context.Background()
		candidate, err := s.Import(ctx, c, Wish{Request: "change greeting", Source: src}, from)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Approve(ctx, c, candidate.ID); err != nil {
			t.Fatal(err)
		}
		p, err := state.AutoLogPath(s.Options.Home)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("source %+v wrote a decision log (%v)", src, err)
		}
	}
}
