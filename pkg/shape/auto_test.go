// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package shape

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type reasoningDelegate struct{}

func (reasoningDelegate) Delegate(_ context.Context, r DelegateRequest) (DelegateResult, error) {
	return DelegateResult{Agent: "binding", Output: "COVERED:\n- greeting\nNOT COVERED:\n- (none)\nWHY:\n- requested wording\nEXPECT:\n- new greeting\nNEXT:\n- edit colors"}, os.WriteFile(filepath.Join(r.Dir, "index.html"), []byte("new greeting"), 0600)
}

func TestHumanWishDecisionLog(t *testing.T) {
	s, caller, _, _, _ := testService(t)
	s.Options.Delegate = reasoningDelegate{}
	ctx := context.Background()
	c, err := s.Wish(ctx, caller, Wish{Request: "change greeting", Todo: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(ctx, caller, c.ID); err != nil {
		t.Fatal(err)
	}
	log := autoLog(t, s)
	if len(log) != 3 {
		t.Fatalf("decisions: %+v", log)
	}
	for i, event := range []string{"trigger", "verdict", "activation"} {
		d := log[i]
		if d.Event != event || d.Problem != "todo:123" || d.Principal != caller.Principal || d.Line != caller.Principal || d.Source != "human" || d.Base != c.BaseRev {
			t.Fatalf("unexpected %s: %+v", event, d)
		}
		if i > 0 && (d.Rev != c.Rev || d.Agent != "binding" || d.Why != "requested wording" || d.Expect != "new greeting") {
			t.Fatalf("missing candidate metadata: %+v", d)
		}
	}
}

func TestClosingBlockReasons(t *testing.T) {
	old := "COVERED:\n- one\nNOT COVERED:\n- two\nNEXT:\n- edit colors"
	for _, extra := range []string{"", "\n## WHY:\n\n- first\n* second\nEXPECT:\n- result"} {
		output := old + extra
		why, expect := ParseReasons(output)
		if extra == "" && (why != "" || expect != "") || extra != "" && (why != "first\nsecond" || expect != "result") {
			t.Fatalf("reasons: %q, %q", why, expect)
		}
		coverage := ParseCoverage(output)
		if coverage == nil || strings.Join(coverage.NotCovered, ",") != "two" || strings.Join(ParseNext(output), ",") != "edit colors" {
			t.Fatalf("sections mixed: %q", output)
		}
	}
}
