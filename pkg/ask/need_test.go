package ask

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// asHuman and asAgent pin who the process is, whatever harness runs the tests.
func asHuman(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_AGENT", "")
	t.Setenv("BASHY_PRINCIPAL", "olivia")
	old := detectAgentTool
	detectAgentTool = func() (string, bool) { return "", false }
	t.Cleanup(func() { detectAgentTool = old })
}

func asAgent(t *testing.T, name string) {
	t.Helper()
	asHuman(t)
	t.Setenv("BASHY_AGENT", name)
}

func isolateNeeds(t *testing.T) {
	t.Helper()
	isolateApprovals(t)
	t.Setenv("HOME", t.TempDir())
	asHuman(t)
}

func raise(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := runApprove(t, append([]string{"request"}, args...)...)
	if err != nil {
		t.Fatalf("request %v: %v (%s)", args, err, errOut)
	}
	return strings.TrimSpace(out)
}

func showNeed(t *testing.T, id string) approvalView {
	t.Helper()
	out, _, err := runApprove(t, "show", id, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v approvalView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("show json: %v\n%s", err, out)
	}
	return v
}

func TestNeedRaiseTypedAndIdempotent(t *testing.T) {
	isolateNeeds(t)
	asAgent(t, "mgr-741")
	id := raise(t, "--kind", "decision", "--title", "Ship the check-in hotfix?",
		"--option", "ship:Ship now", "--option", "hold", "--recommend", "ship",
		"--ref", "sprint:741", "--deadline", "4h", "--risk", "deploy", "--sprint", "741")
	again := raise(t, "--kind", "decision", "--title", "Ship the check-in hotfix?", "--option", "ship", "--option", "hold")
	if again != id {
		t.Fatalf("re-raise made a second request: %s vs %s", again, id)
	}
	v := showNeed(t, id)
	n := v.Need
	if n == nil || n.Kind != "decision" || n.Recommend != "ship" || n.Human != "olivia" || n.From != "mgr-741" ||
		n.Route != RouteHuman || v.NeedState != "open" || len(v.Options) != 2 || v.Sprint != "741" {
		t.Fatalf("unexpected need view: %+v need=%+v", v, n)
	}
	if time.Until(n.Deadline) < 3*time.Hour {
		t.Fatalf("deadline not parsed: %v", n.Deadline)
	}
	if !NeedsHuman(mustFind(t, id), time.Now()) {
		t.Fatal("an open request must be waiting on the human")
	}
}

func TestNeedRaiseRefusesBadInput(t *testing.T) {
	isolateNeeds(t)
	for _, args := range [][]string{
		{"--kind", "vote", "--title", "x"},
		{"--kind", "decision"},
		{"--kind", "decision", "--title", "x", "--option", "a", "--recommend", "b"},
		{"--kind", "info", "--title", "x", "--urgency", "now"},
		{"--kind", "info", "--title", "x", "--deadline", "soon"},
		{"--kind", "info", "--title", "x", "--", "rm", "-rf"},
	} {
		if _, _, err := runApprove(t, append([]string{"request"}, args...)...); err == nil {
			t.Errorf("request %v: want refusal", args)
		}
	}
}

func mustFind(t *testing.T, id string) Request {
	t.Helper()
	r, err := Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNeedHumanAnswersOption(t *testing.T) {
	isolateNeeds(t)
	id := raise(t, "--kind", "decision", "--title", "Pick", "--option", "a", "--option", "b")
	if _, _, err := runApprove(t, "allow", id, "--option", "c"); err == nil {
		t.Fatal("an option the request does not offer must be refused")
	}
	if _, _, err := runApprove(t, "allow", id, "--option", "b", "--reason", "fine"); err != nil {
		t.Fatal(err)
	}
	v := showNeed(t, id)
	if v.Decision == nil || v.Decision.OptionID != "b" || v.Decision.By != "olivia" || v.Decision.OnBehalfOf != "" ||
		v.NeedState != "answered-by-human" || v.Resume != "" {
		t.Fatalf("decision: %+v state %s resume %q", v.Decision, v.NeedState, v.Resume)
	}
	if _, _, err := runApprove(t, "deny", id); !errors.Is(err, ErrDecided) {
		t.Fatalf("second answer: %v, want ErrDecided", err)
	}
}

func TestNeedUndelegatedAgentIsRefused(t *testing.T) {
	isolateNeeds(t)
	id := raise(t, "--kind", "approval", "--title", "Merge it?")
	asAgent(t, "someone")
	if _, _, err := runApprove(t, "allow", id); !errors.Is(err, ErrNotYours) {
		t.Fatalf("agent answer: %v, want ErrNotYours", err)
	}
	asHuman(t)
	t.Setenv("BASHY_PRINCIPAL", "mallory")
	if _, _, err := runApprove(t, "allow", id); !errors.Is(err, ErrNotYours) {
		t.Fatalf("another human: %v, want ErrNotYours", err)
	}
	if _, err := Decide(id, Decision{Verdict: VerdictAllow}, DecideOpts{Caller: Caller{Human: "mallory"}}); !errors.Is(err, ErrNotYours) {
		t.Fatalf("board as another human: %v, want ErrNotYours", err)
	}
	if _, err := Decide(id, Decision{Verdict: VerdictAllow}, DecideOpts{Via: ViaBoard, Caller: Caller{Human: "olivia"}}); err != nil {
		t.Fatalf("board as the human: %v", err)
	}
	if d, _ := LoadDecision(mustFind(t, id).ID); d.Via != ViaBoard || d.By != "olivia" {
		t.Fatalf("board decision: %+v", d)
	}
}

func TestNeedDelegateInScopeOnBehalf(t *testing.T) {
	isolateNeeds(t)
	if _, _, err := runApprove(t, "delegate", "--to", "steward-agent", "--kind", "decision,info", "--sprint", "741"); err != nil {
		t.Fatal(err)
	}
	in := raise(t, "--kind", "decision", "--title", "Review the lifted specs", "--option", "accept", "--option", "rework", "--sprint", "741")
	risky := raise(t, "--kind", "decision", "--title", "Push the security hotfix", "--risk", "security", "--sprint", "741")
	outKind := raise(t, "--kind", "action", "--title", "Rotate the key", "--sprint", "741")
	outSprint := raise(t, "--kind", "decision", "--title", "Other sprint", "--sprint", "700")

	if v := showNeed(t, in); v.Need.Route != RouteDelegate || v.Need.Delegate != "steward-agent" || v.NeedState != "delegated" {
		t.Fatalf("in-scope request not routed to the delegate: %+v %s", v.Need, v.NeedState)
	}
	if NeedsHuman(mustFind(t, in), time.Now()) {
		t.Fatal("a delegated request must not alert the human")
	}
	for _, id := range []string{risky, outKind, outSprint} {
		if v := showNeed(t, id); v.Need.Route != RouteHuman {
			t.Fatalf("%s (%s) must go to the human, got route %s", id, v.Need.Title, v.Need.Route)
		}
	}

	asAgent(t, "steward-agent")
	if _, _, err := runApprove(t, "delegate", "--revoke"); !errors.Is(err, ErrHumanOnly) {
		t.Fatalf("delegate revoking: %v, want ErrHumanOnly", err)
	}
	if _, _, err := runApprove(t, "allow", risky); !errors.Is(err, ErrNotYours) {
		t.Fatalf("delegate on a non-delegable request: %v, want ErrNotYours", err)
	}
	asAgent(t, "intruder")
	if _, _, err := runApprove(t, "allow", in, "--option", "accept"); !errors.Is(err, ErrNotYours) {
		t.Fatalf("non-delegate agent: %v, want ErrNotYours", err)
	}
	asAgent(t, "steward-agent")
	if _, _, err := runApprove(t, "allow", in, "--option", "accept"); err != nil {
		t.Fatalf("delegate in scope: %v", err)
	}
	v := showNeed(t, in)
	if v.Decision.By != "steward-agent" || v.Decision.OnBehalfOf != "olivia" || v.Decision.Via != ViaDelegate ||
		v.NeedState != "answered-by-delegate" {
		t.Fatalf("on-behalf decision: %+v %s", v.Decision, v.NeedState)
	}
	asAgent(t, "steward-agent")
	if _, _, err := runApprove(t, "deny", in, "--override"); !errors.Is(err, ErrOverrideHumanOnly) {
		t.Fatalf("delegate overriding: %v, want ErrHumanOnly", err)
	}

	asHuman(t)
	out, _, err := runApprove(t, "list", "--all", "--on-behalf")
	if err != nil || !strings.Contains(out, "steward-agent for olivia") || strings.Contains(out, "Push the security") {
		t.Fatalf("on-behalf list: %v\n%s", err, out)
	}
	if _, _, err := runApprove(t, "allow", in, "--option", "rework", "--override", "--reason", "not yet"); err != nil {
		t.Fatalf("human override: %v", err)
	}
	v = showNeed(t, in)
	if v.Decision.OptionID != "rework" || v.Decision.By != "olivia" || !strings.Contains(v.Decision.Overrides, "steward-agent") ||
		len(v.Prior) != 1 || v.Prior[0].OnBehalfOf != "olivia" {
		t.Fatalf("override: %+v prior %+v", v.Decision, v.Prior)
	}
}

func TestNeedRevokeEscalatesAtOnce(t *testing.T) {
	isolateNeeds(t)
	if _, err := SetDelegation(Delegation{To: "steward-agent"}); err != nil {
		t.Fatal(err)
	}
	id := raise(t, "--kind", "info", "--title", "FYI")
	if NeedsHuman(mustFind(t, id), time.Now()) {
		t.Fatal("delegated before revoke")
	}
	if _, _, err := runApprove(t, "delegate", "--revoke"); err != nil {
		t.Fatal(err)
	}
	if st := NeedState(mustFind(t, id), time.Now()); st != "escalated" {
		t.Fatalf("after revoke: %s, want escalated", st)
	}
	asAgent(t, "steward-agent")
	if _, _, err := runApprove(t, "allow", id); !errors.Is(err, ErrNotYours) {
		t.Fatalf("revoked delegate answering: %v", err)
	}
	asHuman(t)
	out, _, _ := runApprove(t, "delegate")
	if !strings.Contains(out, "no delegate") {
		t.Fatalf("delegate show after revoke: %s", out)
	}
}

func TestNeedDeadlineEscalatesDelegated(t *testing.T) {
	isolateNeeds(t)
	if _, err := SetDelegation(Delegation{To: "steward-agent"}); err != nil {
		t.Fatal(err)
	}
	id := raise(t, "--kind", "decision", "--title", "Quick one", "--deadline", "1h")
	r := mustFind(t, id)
	if st := NeedState(r, time.Now().Add(2*time.Hour)); st != "escalated" {
		t.Fatalf("past deadline: %s, want escalated", st)
	}
	if !NeedsHuman(r, time.Now().Add(2*time.Hour)) {
		t.Fatal("an escalated request must alert the human")
	}
}

func TestNeedLegacyApprovalUnchanged(t *testing.T) {
	isolateNeeds(t)
	asAgent(t, "any-agent")
	out := raise(t, "--", "rm", "-rf", "build")
	if _, _, err := runApprove(t, "allow", out); err != nil {
		t.Fatalf("legacy approval decide changed: %v", err)
	}
	if v := showNeed(t, out); v.Need != nil || !strings.HasPrefix(v.Resume, "--confirm=") {
		t.Fatalf("legacy view changed: %+v", v)
	}
}

func TestNeedAddressedByKey(t *testing.T) {
	isolateNeeds(t)
	id := raise(t, "--kind", "approval", "--title", "Merge?", "--key", "merge-741")
	if v := showNeed(t, "key:merge-741"); v.ID != id {
		t.Fatalf("key:merge-741 resolved to %s, want %s", v.ID, id)
	}
	if _, _, err := runApprove(t, "allow", "key:merge-741"); err != nil {
		t.Fatal(err)
	}
	if v := showNeed(t, "key:merge-741"); v.ID != id || v.Decision == nil {
		t.Fatalf("decided request by key: %+v", v)
	}
	if _, _, err := runApprove(t, "show", "key:nope"); err == nil {
		t.Fatal("unknown key must fail")
	}
}

func TestNeedAnswerWakesRequester(t *testing.T) {
	isolateNeeds(t)
	var got []string
	old := notifyFn
	notifyFn = func(from, to, subject string) error { got = append(got, from+">"+to+": "+subject); return nil }
	t.Cleanup(func() { notifyFn = old })
	asAgent(t, "mgr-741")
	id := raise(t, "--kind", "decision", "--title", "Go?", "--option", "go", "--key", "go")
	asHuman(t)
	if _, _, err := runApprove(t, "allow", "key:go", "--option", "go"); err != nil {
		t.Fatal(err)
	}
	want := "olivia>mgr-741: approve " + id[:8] + " answered: allow go"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("notifications = %q, want [%q]", got, want)
	}
}

// Risk tags are matched case-insensitively: "Deploy" is the non-delegable
// "deploy" (review finding, Sprint 745).
func TestNeedRiskTagsAreCaseInsensitive(t *testing.T) {
	isolateNeeds(t)
	if _, err := SetDelegation(Delegation{To: "steward-agent"}); err != nil {
		t.Fatal(err)
	}
	id := raise(t, "--kind", "decision", "--title", "Deploy to production?", "--risk", " Deploy ")
	if v := showNeed(t, id); v.Need.Route != RouteHuman || len(v.Need.Risk) != 1 || v.Need.Risk[0] != "deploy" {
		t.Fatalf("mixed-case deploy tag routed to %s with risk %v", v.Need.Route, v.Need.Risk)
	}
	if _, err := SetDelegation(Delegation{To: "steward-agent", AllowRisk: []string{"DEPLOY"}}); err != nil {
		t.Fatal(err)
	}
	id2 := raise(t, "--kind", "decision", "--title", "Deploy staging?", "--risk", "deploy")
	if v := showNeed(t, id2); v.Need.Route != RouteDelegate {
		t.Fatalf("an allowed tag must be delegable regardless of case, got %s", v.Need.Route)
	}
}

// Narrowing a delegation so a delegated request falls out of its scope hands
// that request back to the human instead of leaving it stuck as delegated.
func TestNeedNarrowedDelegationEscalates(t *testing.T) {
	isolateNeeds(t)
	if _, err := SetDelegation(Delegation{To: "steward-agent"}); err != nil {
		t.Fatal(err)
	}
	id := raise(t, "--kind", "action", "--title", "Rotate logs", "--sprint", "741")
	if st := NeedState(mustFind(t, id), time.Now()); st != "delegated" {
		t.Fatalf("before narrowing: %s", st)
	}
	if _, err := SetDelegation(Delegation{To: "steward-agent", Kinds: []string{"decision"}}); err != nil {
		t.Fatal(err)
	}
	r := mustFind(t, id)
	if st := NeedState(r, time.Now()); st != "escalated" || !NeedsHuman(r, time.Now()) {
		t.Fatalf("after narrowing: %s, want escalated and on the board", st)
	}
}

// An override never leaves the request undecided and keeps every prior
// decision; a plain answer after it is still refused.
func TestNeedOverrideKeepsDecided(t *testing.T) {
	isolateNeeds(t)
	id := raise(t, "--kind", "decision", "--title", "Pick", "--option", "a", "--option", "b", "--option", "c")
	if _, _, err := runApprove(t, "allow", id, "--option", "a"); err != nil {
		t.Fatal(err)
	}
	for _, opt := range []string{"b", "c"} {
		if _, _, err := runApprove(t, "allow", id, "--option", opt, "--override"); err != nil {
			t.Fatalf("override %s: %v", opt, err)
		}
		if _, ok := LoadDecision(mustFind(t, id).ID); !ok {
			t.Fatal("request undecided after an override")
		}
	}
	v := showNeed(t, id)
	if v.Decision.OptionID != "c" || len(v.Prior) != 2 || v.Prior[0].OptionID != "a" || v.Prior[1].OptionID != "b" {
		t.Fatalf("decision %+v prior %+v", v.Decision, v.Prior)
	}
	if _, _, err := runApprove(t, "deny", id); !errors.Is(err, ErrDecided) {
		t.Fatalf("plain answer after override: %v, want ErrDecided", err)
	}
	if _, err := os.Stat(filepath.Join(requestDir(mustFind(t, id).ID), "override.lock")); !os.IsNotExist(err) {
		t.Fatalf("override lock left behind: %v", err)
	}
}
