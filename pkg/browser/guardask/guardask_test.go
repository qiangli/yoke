package guardask

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// stub replaces ask.Form/ask.TakeValue for one test.
func stub(t *testing.T, f func(ask.FormSpec) (ask.FormResult, error)) (specs *[]ask.FormSpec, taken *[]string) {
	t.Helper()
	var s []ask.FormSpec
	var tk []string
	oldForm, oldTake := form, take
	form = func(_ context.Context, spec ask.FormSpec) (ask.FormResult, error) {
		s = append(s, spec)
		return f(spec)
	}
	take = func(id string) ([]byte, error) { tk = append(tk, id); return []byte("discarded"), nil }
	t.Cleanup(func() { form, take = oldForm, oldTake })
	return &s, &tk
}

func question() guard.GuardAsk {
	return guard.GuardAsk{
		Site: "acme.typeform.com", Question: "What is your bank routing number?", Reason: "field:label-routing",
		Why: "the field asks for routing", Action: "type", Category: "routing",
		CanProvide: true, CanSelf: true, Timeout: time.Minute,
	}
}

func TestAskMapsEachChoice(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		res  ask.FormResult
		err  error
		want guard.GuardAnswer
	}{
		{ask.FormResult{Choice: ChoiceEnter, ID: id}, nil, guard.GuardAnswer{Choice: guard.ChoiceProvide, AskID: id, Via: "operator"}},
		// The clipboard (S512.7) is provide too: same sink, same reference.
		{ask.FormResult{Choice: ChoiceClipboard, ID: id}, nil, guard.GuardAnswer{Choice: guard.ChoiceProvide, AskID: id, Via: "operator"}},
		{ask.FormResult{Choice: ChoiceEnter}, nil, guard.GuardAnswer{Choice: guard.ChoiceStop, Via: "operator"}},
		{ask.FormResult{Choice: ChoiceSelf}, nil, guard.GuardAnswer{Choice: guard.ChoiceSelf, Via: "operator"}},
		{ask.FormResult{Choice: ChoiceSkip}, nil, guard.GuardAnswer{Choice: guard.ChoiceSkip, Via: "operator"}},
		{ask.FormResult{Choice: ChoiceStop}, nil, guard.GuardAnswer{Choice: guard.ChoiceStop, Via: "operator"}},
		// A refused clipboard shape / declined entry / no desktop: stop.
		{ask.FormResult{}, errors.New("ask: clipboard fails the routing check"), guard.GuardAnswer{Choice: guard.ChoiceStop, Via: "ask-refused"}},
	} {
		specs, _ := stub(t, func(ask.FormSpec) (ask.FormResult, error) { return tc.res, tc.err })
		got, err := Asker{}.Ask(context.Background(), question())
		if err != nil || got != tc.want {
			t.Errorf("%+v/%v: got %+v, %v", tc.res, tc.err, got, err)
		}
		spec := (*specs)[0]
		if !slices.Equal(spec.Choices, []string{ChoiceEnter, ChoiceSelf, ChoiceSkip, ChoiceStop}) {
			t.Errorf("choices = %q", spec.Choices)
		}
		// ask.Form adds "Use clipboard" after "Enter it here" and classifies
		// the answer from the field name: the routing checksum applies.
		if spec.Field.Name != "Routing number" || spec.Field.Kind != "text" || spec.Timeout != time.Minute {
			t.Errorf("field = %+v", spec.Field)
		}
		for _, want := range []string{"acme.typeform.com", "What is your bank routing number?", "field:label-routing", ChoiceClipboard, ChoiceStop} {
			if !strings.Contains(spec.Prompt, want) {
				t.Errorf("prompt lacks %q: %s", want, spec.Prompt)
			}
		}
	}
}

// Without provide only the FIRST choice can carry a value in ask.Form, so
// Stop goes first; a value typed there anyway is consumed, never kept.
func TestNoProvidePutsStopFirstAndDiscards(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	specs, taken := stub(t, func(ask.FormSpec) (ask.FormResult, error) {
		return ask.FormResult{Choice: ChoiceStop, ID: id}, nil
	})
	q := question()
	q.CanProvide, q.CanSelf = false, false
	got, _ := Asker{}.Ask(context.Background(), q)
	if got.Choice != guard.ChoiceStop || got.AskID != "" {
		t.Fatalf("got %+v", got)
	}
	if c := (*specs)[0].Choices; !slices.Equal(c, []string{ChoiceStop, ChoiceSkip}) {
		t.Fatalf("choices = %q", c)
	}
	if !slices.Equal(*taken, []string{id}) {
		t.Fatalf("stray value not discarded: %q", *taken)
	}
}

func TestDone(t *testing.T) {
	for choice, want := range map[string]bool{ChoiceDone: true, ChoiceStop: false, "": false} {
		stub(t, func(ask.FormSpec) (ask.FormResult, error) { return ask.FormResult{Choice: choice}, nil })
		if got, err := (Asker{}).Done(context.Background(), question()); got != want || err != nil {
			t.Errorf("%q: got %v, %v", choice, got, err)
		}
	}
	stub(t, func(ask.FormSpec) (ask.FormResult, error) { return ask.FormResult{}, errors.New("timeout") })
	if got, _ := (Asker{}).Done(context.Background(), question()); got {
		t.Error("an error counted as done")
	}
}

func TestSecretCategoriesUseAMaskedEntry(t *testing.T) {
	specs, _ := stub(t, func(ask.FormSpec) (ask.FormResult, error) { return ask.FormResult{Choice: ChoiceStop}, nil })
	q := question()
	q.Category, q.Secret = "password", true
	_, _ = Asker{}.Ask(context.Background(), q)
	if f := (*specs)[0].Field; f.Kind != "secret" || f.Name != "Password" {
		t.Fatalf("field = %+v", f)
	}
}

// Install wires the guard to pkg/ask: a guarded type now asks instead of
// stopping for want of a human channel.
func TestInstall(t *testing.T) {
	stub(t, func(ask.FormSpec) (ask.FormResult, error) { return ask.FormResult{Choice: ChoiceSkip}, nil })
	t.Setenv(guard.FakeAskEnv, "")
	Install()
	t.Cleanup(func() { guard.SetHumanAsker(nil); guard.SetValueSource(nil) })
	b := backend{}
	res, _ := guard.Execute(context.Background(), b, typeInto("hello"), func(context.Context, wire.Action) (*wire.Result, error) {
		t.Fatal("a guarded field was typed into")
		return nil, nil
	})
	if res.OutcomeClass != guard.ClassSkipped {
		t.Fatalf("got %+v", res)
	}
}

// backend is a page whose every field is a password field.
type backend struct{}

func (backend) InspectField(context.Context, wire.Action) (guard.Field, error) {
	return guard.Field{Guarded: true, Reason: "field:type-password", Question: "Password", Site: "x.test"}, nil
}
func (backend) FocusField(context.Context, wire.Action) error { return nil }
func (backend) FillAdmitted(context.Context, wire.Action, string, []byte) error {
	return errors.New("not offered")
}
func (backend) Site(context.Context) string { return "x.test" }
func (backend) HumanChannel() bool          { return true }

func typeInto(text string) wire.Action {
	return wire.Action{Type: wire.ActionType, Selector: "#pw", Text: text}
}
