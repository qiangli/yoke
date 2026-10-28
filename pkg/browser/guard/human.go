package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// The human path. A refusal raises ONE approval on the bashy approve queue
// naming the site, the question and why it was blocked:
//
//	type-myself  bashy focuses the field in the operator's browser and waits
//	             for a second "done" approval (allow = done, deny = stop)
//	skip         leave this question unanswered
//	stop         stop filling this form (also: deny, no answer, timeout)
//
//	bashy approve allow ID --option type-myself|skip|stop
//	bashy approve deny  ID                                 # = stop
//
// The caller (agent, skill) sees only the outcome; the value never exists on
// this path at all — the human types it straight into the page.

// Choice ids offered on the approval.
const (
	ChoiceTypeMyself = "type-myself"
	ChoiceSkip       = "skip"
	ChoiceStop       = "stop"
	choiceDone       = "done"
)

// Source is the approval source the guard raises under.
const Source = "browser-guard"

// Timeouts for the operator's answer. Variables only so tests can shorten
// them; there is no flag or environment variable.
var (
	DecisionTimeout = 10 * time.Minute
	DoneTimeout     = 15 * time.Minute
)

// FakeAskEnv is the TEST-ONLY seam: fake:type-myself | fake:skip | fake:stop
// | fake:timeout answers the guard's approvals as a scripted operator would.
// It is honoured only inside a `go test` binary (testing.Testing) or a build
// made with -tags bashy_guard_fake; a release build ignores it. Even when
// honoured it cannot let a guarded value through — every fake answer is one
// of the human-path outcomes, none of which fills the field.
const FakeAskEnv = "BASHY_BROWSER_GUARD_ASK"

func fakeAnswer() string {
	if !testing.Testing() && !fakeBuild {
		return ""
	}
	v := strings.TrimSpace(os.Getenv(FakeAskEnv))
	if !strings.HasPrefix(v, "fake:") {
		return ""
	}
	switch c := strings.TrimPrefix(v, "fake:"); c {
	case ChoiceTypeMyself, ChoiceSkip, ChoiceStop, "timeout":
		return c
	}
	return ""
}

func human(ctx context.Context, b Backend, a wire.Action, ref refusal) *wire.Result {
	ref.Question = maskString(ref.Question, loadVault(ctx))
	if !b.HumanChannel() && fakeAnswer() == "" {
		return outcome(ClassStopped, ref, "no-human-channel")
	}
	choice, via := decide(ctx, a, ref)
	switch choice {
	case ChoiceTypeMyself:
		if hasField(a) {
			if err := b.FocusField(ctx, a); err != nil {
				return outcome(ClassStopped, ref, "focus-failed")
			}
		}
		if ok, how := waitDone(ctx, ref); !ok {
			return outcome(ClassStopped, ref, how)
		}
		return outcome(ClassHumanTyped, ref, via)
	case ChoiceSkip:
		return outcome(ClassSkipped, ref, via)
	}
	return outcome(ClassStopped, ref, via)
}

func reasonText(code string) string {
	kind := code[strings.LastIndexByte(code, '-')+1:]
	switch {
	case strings.HasPrefix(code, "value:"):
		names := map[string]string{
			KindSSN: "a Social Security number", KindITIN: "an ITIN", KindEIN: "an EIN",
			KindABA: "a bank routing number", KindCard: "a payment card number",
			KindIBAN: "an IBAN", KindVault: "a secret from the bashy vault",
		}
		if n := names[strings.TrimPrefix(code, "value:")]; n != "" {
			return "the value is " + n
		}
	case code == "field:type-password":
		return "the field is a password field"
	case strings.HasPrefix(code, "field:autocomplete-"):
		return "the field is marked autocomplete=" + strings.TrimPrefix(code, "field:autocomplete-")
	case strings.HasPrefix(code, "field:"):
		return "the field asks for " + kind
	}
	return code
}

func prompt(ref refusal, a wire.Action) string {
	q := ref.Question
	if q == "" {
		q = ref.Matched
	}
	if len(q) > 200 {
		q = q[:200] + "…"
	}
	site := ref.Site
	if site == "" {
		site = "(unknown site)"
	}
	return fmt.Sprintf("bashy browser guard: an agent tried to %s on %s, question %q — blocked because %s (%s). "+
		"The agent will not see what you type. Choose: type-myself (bashy focuses the field in your browser; "+
		"you type it, then answer done), skip (leave this question), stop (stop filling this form; also the "+
		"answer if nobody replies).",
		strings.ReplaceAll(a.Type, "_", " "), site, q, reasonText(ref.Reason), ref.Reason)
}

func decide(ctx context.Context, a wire.Action, ref refusal) (string, string) {
	fake := fakeAnswer()
	r, err := ask.Raise(ask.ApprovalSpec{
		Source:  Source,
		Argv:    []string{"browser", "guard", a.Type, ref.Site, ref.Reason},
		Effects: []string{"human-input"},
		Prompt:  prompt(ref, a),
		Options: []ask.ApprovalOption{
			{ID: ChoiceTypeMyself, Name: "I will type it myself", Kind: "allow_once"},
			{ID: ChoiceSkip, Name: "skip this question", Kind: "allow_once"},
			{ID: ChoiceStop, Name: "stop filling this form", Kind: "reject_once"},
		},
		TTL: DecisionTimeout,
	})
	if err != nil {
		return ChoiceStop, "ask-unavailable"
	}
	timeout := DecisionTimeout
	switch fake {
	case "timeout":
		timeout = 50 * time.Millisecond
	case ChoiceStop:
		_, _ = ask.Decide(r.ID, ask.Decision{Verdict: ask.VerdictDeny, By: "fake-operator"}, ask.DecideOpts{})
	case ChoiceTypeMyself, ChoiceSkip:
		_, _ = ask.Decide(r.ID, ask.Decision{Verdict: ask.VerdictAllow, By: "fake-operator", OptionID: fake}, ask.DecideOpts{})
	}
	d, err := wait(ctx, r.ID, timeout)
	if err != nil {
		return ChoiceStop, "timeout"
	}
	if d.Via == ask.ViaTimeout {
		return ChoiceStop, "timeout"
	}
	if !d.Allowed() {
		return ChoiceStop, "operator"
	}
	switch d.OptionID {
	case ChoiceSkip:
		return ChoiceSkip, "operator"
	case ChoiceStop:
		return ChoiceStop, "operator"
	}
	return ChoiceTypeMyself, "operator"
}

// wait blocks for a decision; on timeout it records the deny (nobody
// answered = stop, never a default fill).
func wait(ctx context.Context, id string, timeout time.Duration) (ask.Decision, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d, err := ask.WaitDecision(wctx, id)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return ask.DenyUnanswered(id, "browser guard: no answer within "+timeout.String()), nil
		}
		return ask.Decision{}, err
	}
	return d, nil
}

// waitDone is the type-myself handshake: the field is focused; the operator
// types and then answers done (allow) or stop (deny).
func waitDone(ctx context.Context, ref refusal) (bool, string) {
	fake := fakeAnswer()
	q := ref.Question
	if len(q) > 200 {
		q = q[:200] + "…"
	}
	r, err := ask.Raise(ask.ApprovalSpec{
		Source:  Source,
		Argv:    []string{"browser", "guard", choiceDone, ref.Site, ref.Reason},
		Effects: []string{"human-input"},
		Prompt: fmt.Sprintf("bashy browser guard: the field for %q on %s is focused in your browser. Type your answer there, "+
			"then allow this (done) — or deny it to stop filling the form. Do not paste the value here.", q, ref.Site),
		Options: []ask.ApprovalOption{
			{ID: choiceDone, Name: "done — I typed it", Kind: "allow_once"},
			{ID: ChoiceStop, Name: "stop filling this form", Kind: "reject_once"},
		},
		TTL: DoneTimeout,
	})
	if err != nil {
		return false, "ask-unavailable"
	}
	timeout := DoneTimeout
	if fake == ChoiceTypeMyself {
		_, _ = ask.Decide(r.ID, ask.Decision{Verdict: ask.VerdictAllow, By: "fake-operator", OptionID: choiceDone}, ask.DecideOpts{})
	}
	d, err := wait(ctx, r.ID, timeout)
	if err != nil || d.Via == ask.ViaTimeout {
		return false, "timeout"
	}
	if !d.Allowed() || d.OptionID == ChoiceStop {
		return false, "operator"
	}
	return true, "operator"
}

// outcome builds the result the caller sees: the class, the reason code and
// where the decision came from. Never the value.
func outcome(class string, ref refusal, via string) *wire.Result {
	res := &wire.Result{OutcomeClass: class, Reason: ref.Reason}
	choice := ""
	switch class {
	case ClassHumanTyped:
		res.Success, res.Content, choice = true, ContentHumanTyped, ChoiceTypeMyself
	case ClassSkipped:
		res.Content, choice = ContentSkipped, ChoiceSkip
		res.Error = fmt.Sprintf("guard: %s refused (%s); the operator skipped this question", ref.Reason, reasonText(ref.Reason))
	default:
		res.Content, choice = ContentStopped, ChoiceStop
		res.Error = fmt.Sprintf("guard: %s refused (%s); stopped (%s) — the form needs a human", ref.Reason, reasonText(ref.Reason), via)
	}
	data, _ := json.Marshal(map[string]string{
		"choice": choice, "via": via, "reason": ref.Reason, "site": ref.Site, "question": ref.Question,
	})
	res.Data = string(data)
	return res
}
