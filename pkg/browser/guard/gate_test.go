package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// fakeBackend scripts the page side: the field verdict a guard_field
// preflight returns, and a record of what reached the page.
type fakeBackend struct {
	field    Field
	fieldErr error
	human    bool

	inspected []wire.Action
	focused   []wire.Action
	executed  []wire.Action
	result    *wire.Result
}

func (b *fakeBackend) InspectField(_ context.Context, a wire.Action) (Field, error) {
	b.inspected = append(b.inspected, a)
	return b.field, b.fieldErr
}

func (b *fakeBackend) FocusField(_ context.Context, a wire.Action) error {
	b.focused = append(b.focused, a)
	return nil
}

func (b *fakeBackend) Site(context.Context) string { return "form.typeform.com" }
func (b *fakeBackend) HumanChannel() bool          { return b.human }

func (b *fakeBackend) next(_ context.Context, a wire.Action) (*wire.Result, error) {
	b.executed = append(b.executed, a)
	if b.result != nil {
		return b.result, nil
	}
	return &wire.Result{Success: true, Content: "typed"}, nil
}

// isolate gives the test its own ask store, bashy home, caches and vault,
// and no vault token, so nothing touches the host.
func isolate(t *testing.T, vault ...string) (askDir, home string) {
	t.Helper()
	root := t.TempDir()
	askDir, home = filepath.Join(root, "ask"), filepath.Join(root, "home")
	t.Setenv("BASHY_ASK_DIR", askDir)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv(FakeAskEnv, "")
	prev := vaultValues
	vaultValues = func(_ context.Context, _ bool, fn func(string)) error {
		for _, v := range vault {
			fn(v)
		}
		return nil
	}
	resetVault()
	t.Cleanup(func() { vaultValues = prev; resetVault() })
	return askDir, home
}

func guardedField(reason, question string) Field {
	return Field{Guarded: true, Reason: reason, Question: question, Matched: `<input type="text" name="q1">`, Site: "form.typeform.com"}
}

func typeAction(text string) wire.Action {
	return wire.Action{Type: wire.ActionType, Selector: "input[name=q1]", Text: text}
}

func run(t *testing.T, b *fakeBackend, a wire.Action) *wire.Result {
	t.Helper()
	res, err := Execute(context.Background(), b, a, b.next)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// assertNoLeak fails if value appears in the result envelope or in any file
// the guard path wrote (the ask record, decisions, the room log).
func assertNoLeak(t *testing.T, value string, res *wire.Result, dirs ...string) {
	t.Helper()
	b, _ := json.Marshal(res)
	if strings.Contains(string(b)+res.Data, value) {
		t.Errorf("refused value leaked into the result: %s", b)
	}
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return nil
			}
			if raw, _ := os.ReadFile(p); strings.Contains(string(raw), value) {
				t.Errorf("refused value leaked into %s", p)
			}
			return nil
		})
	}
}

// (a) A field whose question/label is sensitive is refused BEFORE typing,
// whatever the answer — and the human path's choices decide the outcome.
func TestGuardedFieldTakesTheHumanPath(t *testing.T) {
	cases := []struct {
		fake, class, content string
		code                 int
		focused              bool
	}{
		{"fake:type-myself", ClassHumanTyped, ContentHumanTyped, 0, true},
		{"fake:skip", ClassSkipped, ContentSkipped, 4, false},
		{"fake:stop", ClassStopped, ContentStopped, 5, false},
		{"fake:timeout", ClassStopped, ContentStopped, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.fake, func(t *testing.T) {
			askDir, home := isolate(t)
			t.Setenv(FakeAskEnv, tc.fake)
			b := &fakeBackend{human: true, field: guardedField("field:label-ssn", "What is your Social Security Number?")}
			res := run(t, b, typeAction("Ada Lovelace"))
			if len(b.executed) != 0 {
				t.Fatalf("a guarded field was typed into: %+v", b.executed)
			}
			if res.OutcomeClass != tc.class || res.Content != tc.content || res.Reason != "field:label-ssn" {
				t.Fatalf("outcome = %s/%s/%s, want %s/%s/field:label-ssn", res.OutcomeClass, res.Content, res.Reason, tc.class, tc.content)
			}
			if code, ok := ExitCode(res); !ok || code != tc.code {
				t.Fatalf("exit = %d (%v), want %d", code, ok, tc.code)
			}
			if (len(b.focused) == 1) != tc.focused {
				t.Fatalf("focused = %d, want focused=%v", len(b.focused), tc.focused)
			}
			assertNoLeak(t, "Ada Lovelace", res, askDir, home)
			// The ONE question names the site, the question and the reason.
			rs, err := ask.ListApprovals()
			if err != nil || len(rs) == 0 {
				t.Fatalf("no approval raised: %v", err)
			}
			first := rs[0]
			for _, r := range rs {
				if r.Created.Before(first.Created) {
					first = r
				}
			}
			for _, want := range []string{"form.typeform.com", "Social Security Number", "field:label-ssn", "type-myself", "skip", "stop"} {
				if !strings.Contains(first.Prompt, want) {
					t.Errorf("ask prompt lacks %q: %s", want, first.Prompt)
				}
			}
			if tc.fake == "fake:timeout" {
				if d, ok := ask.LoadDecision(first.ID); !ok || d.Via != ask.ViaTimeout || d.Allowed() {
					t.Errorf("timeout was not recorded as a deny: %+v", d)
				}
			}
		})
	}
}

// (b) A sensitive VALUE typed into an innocuous question is refused by the
// value check — per data type, including a bashy vault secret.
func TestSensitiveValueIsRefusedInAnInnocuousField(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	values := map[string]string{
		"212-48-7731":            "value:ssn",
		"912-70-1234":            "value:itin",
		"94-1234567":             "value:ein",
		"021000021":              "value:aba",
		"4111 1111 1111 1111":    "value:luhn",
		"DE89370400440532013000": "value:iban",
		secret:                   "value:vault",
		"note: " + secret:        "value:vault",
	}
	for v, want := range values {
		t.Run(want+"/"+v, func(t *testing.T) {
			askDir, home := isolate(t, secret)
			t.Setenv(FakeAskEnv, "fake:stop")
			b := &fakeBackend{human: true, field: Field{Site: "form.typeform.com", Question: "Anything else we should know?"}}
			res := run(t, b, typeAction(v))
			if len(b.executed) != 0 {
				t.Fatal("a guarded value reached the page")
			}
			if res.Reason != want || res.OutcomeClass != ClassStopped {
				t.Fatalf("reason/class = %s/%s, want %s/%s", res.Reason, res.OutcomeClass, want, ClassStopped)
			}
			assertNoLeak(t, v, res, askDir, home)
		})
	}
}

// The false-positive page: order numbers, phone numbers, dates, ZIP codes
// and amounts typed into ordinary questions pass straight through.
func TestOrdinaryValuesPassThrough(t *testing.T) {
	isolate(t, "correct-horse-battery-staple")
	for _, v := range []string{"ORD-2026-001234", "7734120098", "(415) 555-0132", "+1 415 555 0132",
		"2026-10-28", "10/28/2026", "94105", "94105-1234", "$1,234.56", "1234.50", "Ada Lovelace"} {
		b := &fakeBackend{human: true, field: Field{Site: "shop.example", Question: "Order number"}}
		res := run(t, b, typeAction(v))
		if len(b.executed) != 1 || b.executed[0].Text != v || !res.Success {
			t.Errorf("%q was blocked: %+v", v, res)
		}
	}
}

// Every value-carrying action is checked, not only type.
func TestValueCheckCoversEveryWritingAction(t *testing.T) {
	isolate(t)
	t.Setenv(FakeAskEnv, "fake:stop")
	actions := []wire.Action{
		{Type: wire.ActionEvaluate, Script: `document.querySelector('#q').value = "212-48-7731"`},
		{Type: wire.ActionClipboardWrite, Text: "4111111111111111"},
		{Type: wire.ActionDispatchEvent, Event: "fill", Detail: `{"routing":"021000021"}`},
		{Type: wire.ActionNavigate, URL: "https://example.com/apply?ssn=212%2048%207731"},
		{Type: wire.ActionSelect, Selector: "#bank", Text: "DE89 3704 0044 0532 0130 00"},
	}
	for _, a := range actions {
		b := &fakeBackend{human: true}
		res := run(t, b, a)
		if len(b.executed) != 0 || res.OutcomeClass != ClassStopped || !strings.HasPrefix(res.Reason, "value:") {
			t.Errorf("%s was not refused: %+v", a.Type, res)
		}
	}
	// Reads are never gated.
	b := &fakeBackend{human: true}
	if res := run(t, b, wire.Action{Type: wire.ActionExtract}); len(b.executed) != 1 || !res.Success {
		t.Error("extract was gated")
	}
}

// A printable keypress into a guarded field is a type by another name.
func TestPrintableKeypressIsFieldChecked(t *testing.T) {
	isolate(t)
	t.Setenv(FakeAskEnv, "fake:skip")
	b := &fakeBackend{human: true, field: Field{Guarded: true, Reason: "field:type-password", Site: "x"}}
	res := run(t, b, wire.Action{Type: wire.ActionKeyboardPress, Key: "7"})
	if len(b.executed) != 0 || res.OutcomeClass != ClassSkipped {
		t.Fatalf("keypress into a password field went through: %+v", res)
	}
	b = &fakeBackend{human: true, field: Field{Guarded: true, Reason: "field:type-password"}}
	run(t, b, wire.Action{Type: wire.ActionKeyboardPress, Key: "Enter"})
	if len(b.inspected) != 0 || len(b.executed) != 1 {
		t.Fatal("a non-printable key was field-checked or blocked")
	}
}

// The field check failing (old extension, page gone) refuses: fail closed.
func TestFieldCheckFailureFailsClosed(t *testing.T) {
	isolate(t)
	b := &fakeBackend{human: true, fieldErr: errors.New(`live: method "guard_field" not advertised by extension v0.8.1`)}
	res := run(t, b, typeAction("hello"))
	if len(b.executed) != 0 || res.Success || !strings.Contains(res.Error, "field check failed") {
		t.Fatalf("type went through without a field check: %+v", res)
	}
}

// No human channel (headless solo) = stopped, without raising an ask nobody
// can answer.
func TestNoHumanChannelStops(t *testing.T) {
	isolate(t)
	b := &fakeBackend{human: false, field: guardedField("field:type-password", "Password")}
	res := run(t, b, typeAction("pw"))
	if res.OutcomeClass != ClassStopped || !strings.Contains(res.Error, "no-human-channel") {
		t.Fatalf("got %+v", res)
	}
	if rs, _ := ask.ListApprovals(); len(rs) != 0 {
		t.Fatal("an ask was raised with no human to answer it")
	}
}

// The REAL queue: an operator answering through `bashy approve` semantics
// (ask.Decide with an option) drives each outcome; type-myself waits for the
// second "done" approval.
func TestOperatorAnswersThroughTheApprovalQueue(t *testing.T) {
	answer := func(t *testing.T, decide func(r ask.Request) ask.Decision, n int) {
		go func() {
			seen := map[string]bool{}
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(seen) < n; {
				rs, _ := ask.PendingApprovals()
				for _, r := range rs {
					if !seen[r.ID] {
						seen[r.ID] = true
						_, _ = ask.Decide(r.ID, decide(r), ask.DecideOpts{})
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	t.Run("type-myself then done", func(t *testing.T) {
		isolate(t)
		answer(t, func(r ask.Request) ask.Decision {
			if r.Approval.Argv[2] == "done" {
				return ask.Decision{Verdict: ask.VerdictAllow, OptionID: "done"}
			}
			return ask.Decision{Verdict: ask.VerdictAllow, OptionID: ChoiceTypeMyself}
		}, 2)
		b := &fakeBackend{human: true, field: guardedField("field:autocomplete-cc-number", "Card number")}
		res := run(t, b, typeAction("4111111111111111"))
		if res.OutcomeClass != ClassHumanTyped || len(b.focused) != 1 || len(b.executed) != 0 {
			t.Fatalf("got %+v focused=%d executed=%d", res, len(b.focused), len(b.executed))
		}
	})
	t.Run("skip", func(t *testing.T) {
		isolate(t)
		answer(t, func(ask.Request) ask.Decision { return ask.Decision{Verdict: ask.VerdictAllow, OptionID: ChoiceSkip} }, 1)
		res := run(t, &fakeBackend{human: true, field: guardedField("field:label-routing", "Routing number")}, typeAction("x"))
		if res.OutcomeClass != ClassSkipped {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("deny is stop", func(t *testing.T) {
		isolate(t)
		answer(t, func(ask.Request) ask.Decision { return ask.Decision{Verdict: ask.VerdictDeny} }, 1)
		res := run(t, &fakeBackend{human: true, field: guardedField("field:label-account", "Bank account")}, typeAction("x"))
		if res.OutcomeClass != ClassStopped {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("done denied is stop", func(t *testing.T) {
		isolate(t)
		answer(t, func(r ask.Request) ask.Decision {
			if r.Approval.Argv[2] == "done" {
				return ask.Decision{Verdict: ask.VerdictDeny}
			}
			return ask.Decision{Verdict: ask.VerdictAllow, OptionID: ChoiceTypeMyself}
		}, 2)
		res := run(t, &fakeBackend{human: true, field: guardedField("field:label-password", "Password")}, typeAction("x"))
		if res.OutcomeClass != ClassStopped {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("no answer is stop", func(t *testing.T) {
		isolate(t)
		prev := DecisionTimeout
		DecisionTimeout = 100 * time.Millisecond
		t.Cleanup(func() { DecisionTimeout = prev })
		res := run(t, &fakeBackend{human: true, field: guardedField("field:label-tax", "Tax ID")}, typeAction("x"))
		if res.OutcomeClass != ClassStopped || !strings.Contains(res.Error, "timeout") {
			t.Fatalf("got %+v", res)
		}
	})
}

// (d)/(4) What a page SHOWS is masked in every result, and structured data
// stays valid JSON.
func TestResultsAreMasked(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	isolate(t, secret)
	b := &fakeBackend{human: true, result: &wire.Result{
		Success:  true,
		Content:  "Confirm: SSN 212-48-7731, card 4111 1111 1111 1111, token " + secret + ", order ORD-2026-001234, call (415) 555-0132",
		Elements: `[1] <input name="routing" value="021000021">`,
		Data:     `{"elements":[{"text":"IBAN DE89370400440532013000","box":{"x":12}}],"total":2}`,
	}}
	res := run(t, b, wire.Action{Type: wire.ActionExtract})
	for _, leaked := range []string{"212-48-7731", "4111 1111 1111 1111", secret, "021000021", "DE89370400440532013000"} {
		if strings.Contains(res.Content+res.Elements+res.Data, leaked) {
			t.Errorf("%q not masked", leaked)
		}
	}
	for _, kept := range []string{"ORD-2026-001234", "(415) 555-0132", "[guarded:ssn]", "[guarded:luhn]", "[guarded:vault]"} {
		if !strings.Contains(res.Content, kept) {
			t.Errorf("content lost %q: %s", kept, res.Content)
		}
	}
	if !json.Valid([]byte(res.Data)) || !strings.Contains(res.Data, "[guarded:iban]") {
		t.Errorf("data masked badly: %s", res.Data)
	}
}

// A question that itself shows a sensitive value (a summary screen) is
// masked before it reaches the ask record.
func TestQuestionTextIsMaskedInTheAsk(t *testing.T) {
	askDir, home := isolate(t)
	t.Setenv(FakeAskEnv, "fake:stop")
	b := &fakeBackend{human: true, field: guardedField("field:label-ssn", "Confirm SSN 212-48-7731")}
	res := run(t, b, typeAction("x"))
	assertNoLeak(t, "212-48-7731", res, askDir, home)
}

// The fake seam is honoured in this test binary — and only because it IS a
// test binary (fake.go: a release build has fakeBuild=false and is not
// testing.Testing()).
func TestFakeSeamIsTestOnly(t *testing.T) {
	if fakeBuild {
		t.Skip("built with bashy_guard_fake")
	}
	t.Setenv(FakeAskEnv, "fake:skip")
	if fakeAnswer() != ChoiceSkip {
		t.Fatal("fake seam not honoured under go test")
	}
	t.Setenv(FakeAskEnv, "fake:allow-the-value")
	if fakeAnswer() != "" {
		t.Fatal("an unknown fake answer was honoured")
	}
}
