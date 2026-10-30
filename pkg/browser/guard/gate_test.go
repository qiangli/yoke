package guard

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	released  []wire.Action
	executed  []wire.Action
	filled    []string // values FillAdmitted typed (the operator's answers)
	fillErr   error
	page      func(string) string // what the page makes of a filled value (nil: keeps it)
	result    *wire.Result
}

func (b *fakeBackend) FillAdmitted(_ context.Context, a wire.Action, _ string, v []byte) (Verify, error) {
	if b.fillErr != nil {
		return Verify{}, b.fillErr
	}
	if a.Text != "" {
		return Verify{}, errors.New("FillAdmitted got an action carrying text")
	}
	b.filled = append(b.filled, string(v))
	field := string(v)
	if b.page != nil {
		field = b.page(field)
	}
	ver := Verify{Verified: VerifiedMismatch, FieldLen: len(field), ValueLen: len(v)}
	if field == string(v) {
		ver.Verified = VerifiedMatch
	}
	return ver, nil
}

func (b *fakeBackend) InspectField(_ context.Context, a wire.Action) (Field, error) {
	b.inspected = append(b.inspected, a)
	return b.field, b.fieldErr
}

func (b *fakeBackend) ReleaseField(_ context.Context, a wire.Action) error {
	b.released = append(b.released, a)
	return nil
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
	t.Setenv(FakeValueEnv, "")
	SetHumanAsker(nil)
	SetValueSource(nil)
	t.Cleanup(func() { SetHumanAsker(nil); SetValueSource(nil) })
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
// whatever the answer — and the human path's choices decide the outcome
// (the fake seam, as S512.6's fixtures drive it).
func TestGuardedFieldTakesTheHumanPath(t *testing.T) {
	cases := []struct {
		fake, class, content string
		code                 int
		focused, filled      bool
	}{
		{"fake:provide", ClassFilledByHuman, ContentFilledByHuman, 0, false, true},
		{"fake:clipboard", ClassFilledByHuman, ContentFilledByHuman, 0, false, true},
		{"fake:self", ClassHumanTyped, ContentHumanTyped, 0, true, false},
		{"fake:type-myself", ClassHumanTyped, ContentHumanTyped, 0, true, false},
		{"fake:skip", ClassSkipped, ContentSkipped, 4, false, false},
		{"fake:stop", ClassStopped, ContentStopped, 5, false, false},
		{"fake:timeout", ClassStopped, ContentStopped, 5, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.fake, func(t *testing.T) {
			askDir, home := isolate(t)
			t.Setenv(FakeAskEnv, tc.fake)
			t.Setenv(FakeValueEnv, "078-05-1120")
			b := &fakeBackend{human: true, field: guardedField("field:label-ssn", "What is your Social Security Number?")}
			res := run(t, b, typeAction("Ada Lovelace"))
			if len(b.executed) != 0 {
				t.Fatalf("the agent's text reached a guarded field: %+v", b.executed)
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
			if tc.filled != (len(b.filled) == 1 && b.filled[0] == "078-05-1120") {
				t.Fatalf("filled = %q, want filled=%v", b.filled, tc.filled)
			}
			if tc.filled && !strings.Contains(res.Data, `"ask_id":"fake`) {
				t.Errorf("filled-by-human lacks the ask id: %s", res.Data)
			}
			if tc.fake == "fake:timeout" && !strings.Contains(res.Error, "timeout") {
				t.Errorf("timeout not named: %s", res.Error)
			}
			assertNoLeak(t, "Ada Lovelace", res, askDir, home)
			assertNoLeak(t, "078-05-1120", res, askDir, home)
		})
	}
}

// scriptedAsker is an installed HumanAsker (what guardask is in a host):
// it records the question and answers as told.
type scriptedAsker struct {
	answer GuardAnswer
	done   bool
	err    error
	asked  []GuardAsk
	values map[string][]byte
}

func (s *scriptedAsker) Ask(_ context.Context, q GuardAsk) (GuardAnswer, error) {
	s.asked = append(s.asked, q)
	return s.answer, s.err
}

func (s *scriptedAsker) Done(context.Context, GuardAsk) (bool, error) { return s.done, nil }

func (s *scriptedAsker) take(id string) ([]byte, error) {
	v, ok := s.values[id]
	if !ok {
		return nil, errors.New("no value")
	}
	delete(s.values, id)
	return v, nil
}

func install(t *testing.T, s *scriptedAsker) {
	t.Helper()
	SetHumanAsker(s)
	SetValueSource(s.take)
}

const askA = "0123456789abcdef0123456789abcdef"

// The ONE question names the site, the question and the reason, and offers
// exactly what this action and browser allow.
func TestTheQuestionNamesSiteQuestionReason(t *testing.T) {
	isolate(t)
	s := &scriptedAsker{answer: GuardAnswer{Choice: ChoiceStop}}
	install(t, s)
	run(t, &fakeBackend{human: true, field: guardedField("field:label-routing", "Bank routing number")}, typeAction("x"))
	q := s.asked[0]
	if q.Site != "form.typeform.com" || q.Question != "Bank routing number" || q.Reason != "field:label-routing" ||
		q.Category != "routing" || q.Why == "" || !q.CanProvide || !q.CanSelf || q.Timeout != DecisionTimeout {
		t.Fatalf("question = %+v", q)
	}
	// Headless: nobody can type into the browser, but the operator can
	// still provide. An evaluate cannot be filled by reference.
	run(t, &fakeBackend{human: false, field: guardedField("field:type-password", "Password")}, typeAction("pw"))
	if q := s.asked[1]; q.CanSelf || !q.CanProvide || !q.Secret || q.Category != "password" {
		t.Fatalf("headless question = %+v", q)
	}
	run(t, &fakeBackend{human: true}, wire.Action{Type: wire.ActionEvaluate, Script: `"212-48-7731"`})
	if q := s.asked[2]; q.CanSelf || q.CanProvide {
		t.Fatalf("evaluate question = %+v", q)
	}
}

// provide: the operator's answer fills the field BY REFERENCE, once per ask
// id and field; the agent's text never reaches the page.
func TestProvideFillsByReferenceOnce(t *testing.T) {
	askDir, home := isolate(t)
	s := &scriptedAsker{answer: GuardAnswer{Choice: ChoiceProvide, AskID: askA}, values: map[string][]byte{askA: []byte("4111 1111 1111 1111")}}
	install(t, s)
	b := &fakeBackend{human: true, field: guardedField("field:autocomplete-cc-number", "Card number")}
	res := run(t, b, typeAction("the agent's guess"))
	if res.OutcomeClass != ClassFilledByHuman || res.Content != ContentFilledByHuman || !res.Success {
		t.Fatalf("got %+v", res)
	}
	if len(b.executed) != 0 || len(b.filled) != 1 || b.filled[0] != "4111 1111 1111 1111" {
		t.Fatalf("executed=%v filled=%q", b.executed, b.filled)
	}
	if !strings.Contains(res.Data, askA) {
		t.Errorf("no ask id in %s", res.Data)
	}
	if !strings.Contains(res.Data, `"verified":"match"`) || !strings.Contains(res.Data, `"field_len":19`) || !strings.Contains(res.Data, `"value_len":19`) {
		t.Errorf("no verification in %s", res.Data)
	}
	assertNoLeak(t, "4111 1111 1111 1111", res, askDir, home)
	assertNoHash(t, "4111 1111 1111 1111", res)
	// The same id again: the admission is spent.
	again := typeAction("")
	again.FromAsk = askA
	s.values[askA] = []byte("4111 1111 1111 1111")
	if res := run(t, b, again); res.OutcomeClass != ClassStopped || len(b.filled) != 1 {
		t.Fatalf("a spent admission filled again: %+v", res)
	}
}

// --from-ask is admitted for ONE field: another target is refused and the
// admission survives for the right one; text alongside is refused.
func TestFromAskIsBoundToTheField(t *testing.T) {
	isolate(t)
	s := &scriptedAsker{values: map[string][]byte{askA: []byte("hunter2")}}
	install(t, s)
	b := &fakeBackend{human: true}
	a := typeAction("")
	a.FromAsk = askA
	if res := run(t, b, a); res.OutcomeClass != ClassStopped || res.Reason != "from-ask:not-admitted" {
		t.Fatalf("an unadmitted id filled: %+v", res)
	}
	if err := admit(askA, refusal{Reason: "field:type-password", Site: "x"}, a); err != nil {
		t.Fatal(err)
	}
	other := a
	other.Selector = "input[name=q2]"
	if res := run(t, b, other); res.OutcomeClass != ClassStopped || len(b.filled) != 0 {
		t.Fatalf("another field took the answer: %+v", res)
	}
	withText := a
	withText.Text = "hunter2"
	if res := run(t, b, withText); res.Success || len(b.filled) != 0 {
		t.Fatalf("--from-ask with text went through: %+v", res)
	}
	if res := run(t, b, a); res.OutcomeClass != ClassFilledByHuman || len(b.filled) != 1 {
		t.Fatalf("the admitted field was not filled: %+v", res)
	}
}

// assertNoHash fails if a common digest of value (hex or base64) appears in
// the result: a hash of an SSN-class value is as good as the value.
func assertNoHash(t *testing.T, value string, res *wire.Result) {
	t.Helper()
	b, _ := json.Marshal(res)
	out := string(b) + res.Data
	s1, s256, m5 := sha1.Sum([]byte(value)), sha256.Sum256([]byte(value)), md5.Sum([]byte(value))
	for _, d := range [][]byte{s1[:], s256[:], m5[:]} {
		for _, enc := range []string{hex.EncodeToString(d), hex.EncodeToString(d[:8]), base64.StdEncoding.EncodeToString(d), base64.RawURLEncoding.EncodeToString(d)} {
			if strings.Contains(out, enc) {
				t.Errorf("a digest of the value is in the result: %s", out)
			}
		}
	}
}

// The fill is VERIFIED: a page that rewrites or truncates the operator's
// answer turns the fill into guard:fill-mismatch (exit 6, a failure) with
// only the verdict and the two lengths — never the value, never a hash.
func TestProvideFillIsVerified(t *testing.T) {
	cases := map[string]struct {
		page          func(string) string
		verified      string
		class         string
		fieldLen, val int
	}{
		"accepts":  {nil, VerifiedMatch, ClassFilledByHuman, 11, 11},
		"truncate": {func(s string) string { return s[:9] }, VerifiedMismatch, ClassFillMismatch, 9, 11},
		"rewrite":  {func(s string) string { return strings.ReplaceAll(s, "-", "") }, VerifiedMismatch, ClassFillMismatch, 9, 11},
		"rejects":  {func(string) string { return "" }, VerifiedMismatch, ClassFillMismatch, 0, 11},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			askDir, home := isolate(t)
			t.Setenv(FakeAskEnv, "fake:provide")
			t.Setenv(FakeValueEnv, "078-05-1120")
			b := &fakeBackend{human: true, page: tc.page, field: guardedField("field:label-ssn", "Social Security Number")}
			res := run(t, b, typeAction("Ada Lovelace"))
			if res.OutcomeClass != tc.class {
				t.Fatalf("outcome = %+v, want %s", res, tc.class)
			}
			var data struct {
				Verified string `json:"verified"`
				FieldLen int    `json:"field_len"`
				ValueLen int    `json:"value_len"`
				AskID    string `json:"ask_id"`
			}
			if err := json.Unmarshal([]byte(res.Data), &data); err != nil {
				t.Fatal(err)
			}
			if data.Verified != tc.verified || data.FieldLen != tc.fieldLen || data.ValueLen != tc.val || data.AskID == "" {
				t.Fatalf("data = %s", res.Data)
			}
			code, _ := ExitCode(res)
			if tc.class == ClassFillMismatch {
				if res.Success || res.Content != ContentFillMismatch || code != ExitFillMismatch || res.Error == "" {
					t.Fatalf("a mismatched fill did not fail: %+v (exit %d)", res, code)
				}
			} else if !res.Success || res.Content != ContentFilledByHuman || code != 0 {
				t.Fatalf("a matched fill failed: %+v (exit %d)", res, code)
			}
			for _, v := range []string{"078-05-1120", "078051120", "078-05-11"} {
				assertNoLeak(t, v, res, askDir, home)
				assertNoHash(t, v, res)
			}
		})
	}
}

// A failed fill stops — and says nothing about the value.
func TestProvideFillFailureStops(t *testing.T) {
	isolate(t)
	install(t, &scriptedAsker{answer: GuardAnswer{Choice: ChoiceProvide, AskID: askA}, values: map[string][]byte{askA: []byte("212-48-7731")}})
	b := &fakeBackend{human: true, fillErr: errors.New("boom 212-48-7731"), field: guardedField("field:label-ssn", "SSN")}
	res := run(t, b, typeAction("x"))
	if res.OutcomeClass != ClassStopped || !strings.Contains(res.Error, "fill-failed") {
		t.Fatalf("got %+v", res)
	}
	assertNoLeak(t, "212-48-7731", res)
	if strings.Contains(res.Data, `"cause"`) {
		t.Fatalf("a plain error became a cause: %s", res.Data)
	}
}

// A FillFailure's cause is reported as data.cause — only when it is a code.
func TestProvideFillFailureCause(t *testing.T) {
	for cause, want := range map[string]bool{"tab-gone": true, "not-connected": true, "212-48-7731": false, "Ada Lovelace": false, "": false} {
		isolate(t)
		install(t, &scriptedAsker{answer: GuardAnswer{Choice: ChoiceProvide, AskID: askA}, values: map[string][]byte{askA: []byte("212-48-7731")}})
		b := &fakeBackend{human: true, fillErr: FillFailure{Cause: cause}, field: guardedField("field:label-ssn", "SSN")}
		res := run(t, b, typeAction("x"))
		if res.OutcomeClass != ClassStopped || !strings.Contains(res.Data, `"via":"fill-failed"`) {
			t.Fatalf("%q: got %+v", cause, res)
		}
		if got := strings.Contains(res.Data, `"cause":"`+cause+`"`); got != want {
			t.Fatalf("%q: data = %s", cause, res.Data)
		}
		assertNoLeak(t, "212-48-7731", res)
	}
}

// The live hub's check on a raw guard_fill: only a CLAIMED admission for the
// same field, once.
func TestConsumeClaimedIsOncePerField(t *testing.T) {
	isolate(t)
	a := typeAction("")
	if err := ConsumeClaimed(askA, a); err == nil {
		t.Fatal("a raw guard_fill without an admission was accepted")
	}
	if err := admit(askA, refusal{}, a); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeClaimed(askA, a); err == nil {
		t.Fatal("an admitted-but-unclaimed id was accepted (the guard claims before it fills)")
	}
	if _, err := claim(askA, a); err != nil {
		t.Fatal(err)
	}
	other := a
	other.Selector = "#other"
	if err := ConsumeClaimed(askA, other); err == nil {
		t.Fatal("a claim was accepted for another field")
	}
	if _, err := claim(askA, a); err == nil {
		t.Fatal("an admission was claimed twice")
	}
	if err := ConsumeClaimed("../../etc/passwd", a); err == nil {
		t.Fatal("a path-like id was accepted")
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

// No asker installed = no human channel: stopped, without asking anyone.
func TestNoHumanChannelStops(t *testing.T) {
	isolate(t)
	b := &fakeBackend{human: true, field: guardedField("field:type-password", "Password")}
	res := run(t, b, typeAction("pw"))
	if res.OutcomeClass != ClassStopped || !strings.Contains(res.Error, "no-human-channel") || len(b.executed) != 0 {
		t.Fatalf("got %+v", res)
	}
}

// An installed asker drives each outcome; self waits for Done.
func TestInstalledAskerDrivesEachOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     *scriptedAsker
		class string
	}{
		{"self then done", &scriptedAsker{answer: GuardAnswer{Choice: ChoiceSelf}, done: true}, ClassHumanTyped},
		{"self then stop", &scriptedAsker{answer: GuardAnswer{Choice: ChoiceSelf}, done: false}, ClassStopped},
		{"skip", &scriptedAsker{answer: GuardAnswer{Choice: ChoiceSkip}}, ClassSkipped},
		{"stop", &scriptedAsker{answer: GuardAnswer{Choice: ChoiceStop}}, ClassStopped},
		{"timeout", &scriptedAsker{answer: GuardAnswer{Choice: ChoiceStop, Via: "timeout"}}, ClassStopped},
		{"ask error", &scriptedAsker{err: errors.New("no desktop")}, ClassStopped},
		{"provide without id", &scriptedAsker{answer: GuardAnswer{Choice: ChoiceProvide}}, ClassStopped},
		{"unknown choice", &scriptedAsker{answer: GuardAnswer{Choice: "fill-it-anyway"}}, ClassStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			install(t, tc.s)
			b := &fakeBackend{human: true, field: guardedField("field:label-account", "Bank account")}
			res := run(t, b, typeAction("x"))
			if res.OutcomeClass != tc.class || len(b.executed) != 0 || len(b.filled) != 0 {
				t.Fatalf("got %+v executed=%d filled=%d", res, len(b.executed), len(b.filled))
			}
		})
	}
	// self is not honoured where it was not offered (headless).
	isolate(t)
	install(t, &scriptedAsker{answer: GuardAnswer{Choice: ChoiceSelf}, done: true})
	b := &fakeBackend{human: false, field: guardedField("field:label-account", "Bank account")}
	if res := run(t, b, typeAction("x")); res.OutcomeClass != ClassStopped || len(b.focused) != 0 {
		t.Fatalf("self honoured headless: %+v", res)
	}
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
	if !strings.Contains(res.Content, secret) || strings.Contains(res.Content, "[guarded:vault]") {
		t.Errorf("an extract read must not mask a vault value: %s", res.Content)
	}
	for _, kept := range []string{"212-48-7731", "4111 1111 1111 1111", "ORD-2026-001234", "(415) 555-0132"} {
		if !strings.Contains(res.Content, kept) {
			t.Errorf("read content lost %q: %s", kept, res.Content)
		}
	}
	if !strings.Contains(res.Elements, "021000021") || !json.Valid([]byte(res.Data)) || !strings.Contains(res.Data, "DE89370400440532013000") {
		t.Errorf("validator-shaped values must read unmasked: %s %s", res.Elements, res.Data)
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
	if fakeAnswer() != string(ChoiceSkip) {
		t.Fatal("fake seam not honoured under go test")
	}
	t.Setenv(FakeAskEnv, "fake:allow-the-value")
	if fakeAnswer() != "" {
		t.Fatal("an unknown fake answer was honoured")
	}
}

// clipboard_read is a read the agent can make through the extension: what
// the clipboard holds is masked like page text, per data type and for a
// vault secret; an ordinary clipboard passes unchanged.
func TestClipboardReadIsMasked(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	isolate(t, secret)
	for _, v := range []string{"212-48-7731", "4111 1111 1111 1111", "DE89370400440532013000"} {
		b := &fakeBackend{result: &wire.Result{Success: true, Data: v}}
		if res := run(t, b, wire.Action{Type: wire.ActionClipboardRead}); res.Data != v {
			t.Errorf("clipboard %q changed: %q", v, res.Data)
		}
	}
	b := &fakeBackend{result: &wire.Result{Success: true, Data: secret}}
	if res := run(t, b, wire.Action{Type: wire.ActionClipboardRead}); res.Data != "[guarded:vault]" {
		t.Errorf("vault clipboard not masked: %q", res.Data)
	}
	b = &fakeBackend{result: &wire.Result{Success: true, Data: "ORD-2026-001234 (415) 555-0132"}}
	if res := run(t, b, wire.Action{Type: wire.ActionClipboardRead}); res.Data != "ORD-2026-001234 (415) 555-0132" {
		t.Errorf("ordinary clipboard changed: %q", res.Data)
	}
}

// The human-focus outline is cleared after the operator's Done and after a
// focus that ends in Stop; nothing is released when nothing was focused.
func TestSelfFocusIsReleased(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		released int
	}{{"fake:type-myself", 1}, {"fake:skip", 0}, {"fake:stop", 0}} {
		isolate(t)
		t.Setenv(FakeAskEnv, tc.answer)
		b := &fakeBackend{human: true, field: guardedField("field:label-ssn", "SSN")}
		run(t, b, typeAction("x"))
		if len(b.released) != tc.released {
			t.Errorf("%s: released %d, want %d", tc.answer, len(b.released), tc.released)
		}
	}
}

// Reads (extract/text/evaluate) hand a vault value over; a write carrying it
// is still refused.
func TestReadsUnmaskedWritesStillMatchVault(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	isolate(t, secret)
	for _, typ := range []string{wire.ActionExtract, "text", wire.ActionEvaluate} {
		b := &fakeBackend{human: true, result: &wire.Result{Success: true, Content: "v " + secret, Data: secret}}
		res := run(t, b, wire.Action{Type: typ})
		if !strings.Contains(res.Content, secret) || res.Data != secret {
			t.Errorf("%s masked a read: %+v", typ, res)
		}
	}
}

// A dispatch_event that names a field and carries a payload is a write into
// that field: the field check gates it. Window/document dispatches and
// payload-free events on a field are not field writes.
func TestFieldTargetedDispatchIsFieldChecked(t *testing.T) {
	isolate(t)
	t.Setenv(FakeAskEnv, "fake:skip")
	b := &fakeBackend{human: true, field: Field{Guarded: true, Reason: "field:type-password", Site: "x"}}
	res := run(t, b, wire.Action{Type: wire.ActionDispatchEvent, Event: "fill", Selector: "#pw", Detail: `{"v":"x"}`})
	if len(b.executed) != 0 || len(b.inspected) != 1 || res.OutcomeClass != ClassSkipped {
		t.Fatalf("field-targeted dispatch went through: executed=%d inspected=%d %+v", len(b.executed), len(b.inspected), res)
	}
	for _, a := range []wire.Action{
		{Type: wire.ActionDispatchEvent, Event: "fill", Detail: `{"v":"x"}`},
		{Type: wire.ActionDispatchEvent, Event: "fill", Selector: "document", Detail: `{"v":"x"}`},
		{Type: wire.ActionDispatchEvent, Event: "focus", Selector: "#pw"},
	} {
		b := &fakeBackend{human: true, field: Field{Guarded: true, Reason: "field:type-password"}}
		run(t, b, a)
		if len(b.executed) != 1 || len(b.inspected) != 0 {
			t.Errorf("%+v: executed=%d inspected=%d", a, len(b.executed), len(b.inspected))
		}
	}
}
