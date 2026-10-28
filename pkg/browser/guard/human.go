package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/browser/wire"
	"github.com/qiangli/yoke/pkg/sensitive"
)

// The human path (shared contract v2). A refusal asks the operator ONE
// question naming the site, the question text and why it was blocked:
//
//	provide  the operator answers in bashy's own dialog ("Enter it here" or
//	         "Use clipboard"); the answer is held under an ask id and the
//	         guard fills the field BY REFERENCE (admit.go) — the value never
//	         reaches the agent, its stdout, receipts or logs
//	self     bashy focuses the field in the operator's browser; the human
//	         types it there and answers "done"
//	skip     leave this question unanswered
//	stop     stop filling this form (also: no answer, timeout, any error)
//
// The question is asked through a HumanAsker the host installs at init
// (pkg/browser/guardask adapts pkg/ask's native forms); this package does
// not import pkg/ask. With no asker installed there is no human channel and
// every refusal stops.

// GuardChoice is the operator's answer to a guard question.
type GuardChoice string

const (
	ChoiceProvide GuardChoice = "provide"
	ChoiceSelf    GuardChoice = "self"
	ChoiceSkip    GuardChoice = "skip"
	ChoiceStop    GuardChoice = "stop"
)

// GuardAsk is the question put to the operator. It never carries the
// refused value.
type GuardAsk struct {
	Site     string // host of the page ("" when unknown)
	Question string // the visible question/label text, masked
	Reason   string // reason code: field:label-ssn, value:luhn, …
	Why      string // the reason in words
	Action   string // the refused action type
	// Category is the pkg/sensitive clipboard category the answer must
	// have ("ssn", "routing", "card", "password", …; "" when unknown).
	Category string
	// Secret: the answer is a credential (masked entry).
	Secret bool
	// CanProvide: the operator may answer in the dialog (only a type into
	// a field can be filled by reference). CanSelf: the operator can type
	// it into this browser (false for a headless Chrome).
	CanProvide, CanSelf bool
	Timeout             time.Duration
}

// GuardAnswer is the operator's choice. AskID is set only for provide: the
// opaque id a ValueSource resolves.
type GuardAnswer struct {
	Choice GuardChoice
	AskID  string
	Via    string // where the answer came from ("operator", "timeout", …)
}

// HumanAsker asks the operator. Ask raises the one question; Done waits for
// the operator to finish typing into the focused field (self) and reports
// false for stop / no answer.
type HumanAsker interface {
	Ask(ctx context.Context, q GuardAsk) (GuardAnswer, error)
	Done(ctx context.Context, q GuardAsk) (bool, error)
}

// ValueSource resolves a provide answer's ask id to the operator's value,
// at most once (the host adapter uses ask.TakeValue).
type ValueSource func(askID string) ([]byte, error)

var (
	humanMu     sync.RWMutex
	humanAsker  HumanAsker
	valueSource ValueSource
)

// SetHumanAsker installs the host's asker (nil removes it).
func SetHumanAsker(h HumanAsker) {
	humanMu.Lock()
	humanAsker = h
	humanMu.Unlock()
}

// SetValueSource installs the host's value source (nil removes it).
func SetValueSource(v ValueSource) {
	humanMu.Lock()
	valueSource = v
	humanMu.Unlock()
}

// DecisionTimeout bounds the operator's answer. A variable only so tests can
// shorten it; there is no flag or environment variable.
var DecisionTimeout = 10 * time.Minute

// FakeAskEnv is the TEST-ONLY seam: fake:provide | fake:clipboard | fake:self
// (alias fake:type-myself) | fake:skip | fake:stop | fake:timeout answers the
// guard's question as a scripted operator would. It is honoured only inside
// a `go test` binary (testing.Testing) or a build made with -tags
// bashy_guard_fake; a release build ignores it. fake:provide/clipboard
// "provides" FakeValueEnv's value (default "human-provided") by reference,
// exactly as the real path does.
const (
	FakeAskEnv   = "BASHY_BROWSER_GUARD_ASK"
	FakeValueEnv = "BASHY_BROWSER_GUARD_ASK_VALUE"
)

func fakeAnswer() string {
	if !testing.Testing() && !fakeBuild {
		return ""
	}
	v := strings.TrimSpace(os.Getenv(FakeAskEnv))
	if !strings.HasPrefix(v, "fake:") {
		return ""
	}
	switch c := strings.TrimPrefix(v, "fake:"); c {
	case "provide", "clipboard":
		return string(ChoiceProvide)
	case "self", "type-myself":
		return string(ChoiceSelf)
	case string(ChoiceSkip), string(ChoiceStop), "timeout":
		return c
	}
	return ""
}

// fakeAsker is the scripted operator behind FakeAskEnv.
type fakeAsker struct{ answer string }

var fakeValues sync.Map // ask id -> value, one take each

func (f fakeAsker) Ask(ctx context.Context, q GuardAsk) (GuardAnswer, error) {
	switch f.answer {
	case "timeout":
		return GuardAnswer{Choice: ChoiceStop, Via: "timeout"}, nil
	case string(ChoiceProvide):
		v := os.Getenv(FakeValueEnv)
		if v == "" {
			v = "human-provided"
		}
		id := fmt.Sprintf("fake%028x", time.Now().UnixNano())
		fakeValues.Store(id, []byte(v))
		return GuardAnswer{Choice: ChoiceProvide, AskID: id, Via: "fake-operator"}, nil
	}
	return GuardAnswer{Choice: GuardChoice(f.answer), Via: "fake-operator"}, nil
}

func (fakeAsker) Done(context.Context, GuardAsk) (bool, error) { return true, nil }

func fakeValue(id string) ([]byte, error) {
	v, ok := fakeValues.LoadAndDelete(id)
	if !ok {
		return nil, fmt.Errorf("guard: no live value for this ask")
	}
	return v.([]byte), nil
}

// seams returns the asker and value source in force: the fake (test builds
// only) or what the host installed.
func seams() (HumanAsker, ValueSource) {
	if f := fakeAnswer(); f != "" {
		return fakeAsker{f}, fakeValue
	}
	humanMu.RLock()
	defer humanMu.RUnlock()
	return humanAsker, valueSource
}

func human(ctx context.Context, b Backend, a wire.Action, ref refusal) *wire.Result {
	ref.Question = maskString(ref.Question, loadVault(ctx))
	asker, values := seams()
	if asker == nil {
		return outcome(ClassStopped, ref, "no-human-channel", "")
	}
	category, secret := categoryOf(ref.Reason)
	q := GuardAsk{
		Site: ref.Site, Question: clip(ref.Question), Reason: ref.Reason, Why: reasonText(ref.Reason),
		Action: a.Type, Category: category, Secret: secret,
		CanProvide: a.Type == wire.ActionType && values != nil,
		CanSelf:    b.HumanChannel() && hasField(a),
		Timeout:    DecisionTimeout,
	}
	ans, err := asker.Ask(ctx, q)
	if err != nil {
		return outcome(ClassStopped, ref, "ask-unavailable", "")
	}
	via := ans.Via
	if via == "" {
		via = "operator"
	}
	switch ans.Choice {
	case ChoiceProvide:
		if !q.CanProvide || ans.AskID == "" {
			return outcome(ClassStopped, ref, "provide-not-offered", "")
		}
		if err := admit(ans.AskID, ref, a); err != nil {
			return outcome(ClassStopped, ref, "admit-failed", "")
		}
		a.FromAsk, a.Text = ans.AskID, ""
		return fillFromAsk(ctx, b, a)
	case ChoiceSelf:
		if !q.CanSelf {
			return outcome(ClassStopped, ref, "self-not-offered", "")
		}
		if err := b.FocusField(ctx, a); err != nil {
			return outcome(ClassStopped, ref, "focus-failed", "")
		}
		if ok, err := asker.Done(ctx, q); err != nil || !ok {
			how := "operator"
			if err != nil {
				how = "timeout"
			}
			return outcome(ClassStopped, ref, how, "")
		}
		return outcome(ClassHumanTyped, ref, via, "")
	case ChoiceSkip:
		return outcome(ClassSkipped, ref, via, "")
	}
	return outcome(ClassStopped, ref, via, "")
}

func clip(q string) string {
	if len(q) > 200 {
		return q[:200] + "…"
	}
	return q
}

// categoryOf maps a reason code to the pkg/sensitive clipboard category the
// operator's answer must have, and whether it is a credential.
func categoryOf(reason string) (string, bool) {
	switch reason {
	case "value:" + sensitive.KindSSN:
		return "ssn", false
	case "value:" + sensitive.KindITIN:
		return "itin", false
	case "value:" + sensitive.KindEIN:
		return "ein", false
	case "value:" + sensitive.KindABA:
		return "routing", false
	case "value:" + sensitive.KindCard, "field:autocomplete-cc-number":
		return "card", false
	case "value:" + sensitive.KindIBAN:
		return "iban", false
	case "value:" + KindVault:
		return "secret", true
	case "field:type-password", "field:autocomplete-current-password", "field:autocomplete-new-password":
		return "password", true
	case "field:autocomplete-one-time-code":
		return "otp", true
	case "field:autocomplete-username":
		return "username", false
	}
	switch reason[strings.LastIndexByte(reason, '-')+1:] {
	case "ssn":
		return "ssn", false
	case "ein":
		return "ein", false
	case "routing":
		return "routing", false
	case "card":
		return "card", false
	case "account":
		return "account", false
	case "password":
		return "password", true
	case "otp":
		return "otp", true
	case "username":
		return "username", false
	}
	return "", false
}

func reasonText(code string) string {
	kind := code[strings.LastIndexByte(code, '-')+1:]
	switch {
	case strings.HasPrefix(code, "value:"):
		names := map[string]string{
			sensitive.KindSSN: "a Social Security number", sensitive.KindITIN: "an ITIN", sensitive.KindEIN: "an EIN",
			sensitive.KindABA: "a bank routing number", sensitive.KindCard: "a payment card number",
			sensitive.KindIBAN: "an IBAN", KindVault: "a secret from the bashy vault",
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

// outcome builds the result the caller sees: the class, the reason code,
// where the decision came from and (filled-by-human) the opaque ask id.
// Never the value.
func outcome(class string, ref refusal, via, askID string) *wire.Result {
	res := &wire.Result{OutcomeClass: class, Reason: ref.Reason}
	var choice GuardChoice
	switch class {
	case ClassFilledByHuman:
		res.Success, res.Content, choice = true, ContentFilledByHuman, ChoiceProvide
	case ClassHumanTyped:
		res.Success, res.Content, choice = true, ContentHumanTyped, ChoiceSelf
	case ClassSkipped:
		res.Content, choice = ContentSkipped, ChoiceSkip
		res.Error = fmt.Sprintf("guard: %s refused (%s); the operator skipped this question", ref.Reason, reasonText(ref.Reason))
	case ClassFillMismatch:
		res.Content, choice = ContentFillMismatch, ChoiceProvide
		res.Error = fmt.Sprintf("guard: %s refused (%s); the operator's answer was filled but the field does not hold it (the page rewrote, truncated or rejected it) — the form needs a human", ref.Reason, reasonText(ref.Reason))
	default:
		res.Content, choice = ContentStopped, ChoiceStop
		res.Error = fmt.Sprintf("guard: %s refused (%s); stopped (%s) — the form needs a human", ref.Reason, reasonText(ref.Reason), via)
	}
	fields := map[string]any{
		"choice": string(choice), "via": via, "reason": ref.Reason, "site": ref.Site, "question": ref.Question,
	}
	if askID != "" {
		fields["ask_id"] = askID
	}
	data, _ := json.Marshal(fields)
	res.Data = string(data)
	return res
}

// withVerify adds a by-reference fill's verification to res.Data:
// verified, field_len, value_len. Nothing else of the fill is reported.
func withVerify(res *wire.Result, v Verify) *wire.Result {
	var fields map[string]any
	if json.Unmarshal([]byte(res.Data), &fields) != nil {
		fields = map[string]any{}
	}
	fields["verified"], fields["field_len"], fields["value_len"] = v.Verified, v.FieldLen, v.ValueLen
	data, _ := json.Marshal(fields)
	res.Data = string(data)
	return res
}
