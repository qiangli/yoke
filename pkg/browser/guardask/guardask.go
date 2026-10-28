// Package guardask is the host adapter between bashy browser's
// sensitive-input guard (pkg/browser/guard) and the operator's native ask
// forms (pkg/ask): guard.HumanAsker over ask.Form, guard.ValueSource over
// ask.TakeValue. The guard does not import pkg/ask; a host calls Install at
// init (cmds/browser does), so every bashy browser surface gets it.
//
// The ONE question offers, in order:
//
//	Enter it here          → provide (the value lands in ask's 0600 sink;
//	Use clipboard          → provide  only its id comes back)
//	I will type it myself  → self
//	Skip this question     → skip
//	Stop                   → stop (also: dismissed, timeout, any error)
//
// ask.Form lets only its FIRST choice carry a value. When the guard cannot
// fill by reference (not a type), "Stop" goes first so no other choice can
// carry one; a value typed there anyway is taken and discarded.
package guardask

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/browser/guard"
)

// Choice labels on the operator's form.
const (
	ChoiceEnter     = "Enter it here"
	ChoiceClipboard = "Use clipboard"
	ChoiceSelf      = "I will type it myself"
	ChoiceSkip      = "Skip this question"
	ChoiceStop      = "Stop"
	ChoiceDone      = "Done — I typed it"
)

// Install makes pkg/ask the guard's human channel.
func Install() {
	guard.SetHumanAsker(Asker{})
	guard.SetValueSource(ask.TakeValue)
}

// Asker is guard.HumanAsker over ask.Form.
type Asker struct{}

var _ guard.HumanAsker = Asker{}

// form and take are ask.Form / ask.TakeValue (test seams).
var (
	form = ask.Form
	take = ask.TakeValue
)

func (Asker) Ask(ctx context.Context, q guard.GuardAsk) (guard.GuardAnswer, error) {
	var choices []string
	if q.CanProvide {
		choices = append(choices, ChoiceEnter)
	} else {
		choices = append(choices, ChoiceStop)
	}
	if q.CanSelf {
		choices = append(choices, ChoiceSelf)
	}
	choices = append(choices, ChoiceSkip)
	if q.CanProvide {
		choices = append(choices, ChoiceStop)
	}
	kind := "text"
	if q.Secret {
		kind = "secret"
	}
	res, err := form(ctx, ask.FormSpec{
		Title:   "bashy browser guard",
		Prompt:  Prompt(q),
		Field:   ask.FieldSpec{Name: FieldName(q.Category), Kind: kind},
		Choices: choices,
		Timeout: q.Timeout,
	})
	if err != nil {
		// A refused clipboard shape, a declined entry, no desktop: the
		// error never carries the value, and the answer is stop.
		return guard.GuardAnswer{Choice: guard.ChoiceStop, Via: "ask-refused"}, nil
	}
	switch res.Choice {
	case ChoiceEnter, ChoiceClipboard:
		if res.ID == "" {
			return guard.GuardAnswer{Choice: guard.ChoiceStop, Via: "operator"}, nil
		}
		return guard.GuardAnswer{Choice: guard.ChoiceProvide, AskID: res.ID, Via: "operator"}, nil
	case ChoiceSelf:
		return guard.GuardAnswer{Choice: guard.ChoiceSelf, Via: "operator"}, nil
	case ChoiceSkip:
		return guard.GuardAnswer{Choice: guard.ChoiceSkip, Via: "operator"}, nil
	}
	discard(res.ID)
	return guard.GuardAnswer{Choice: guard.ChoiceStop, Via: "operator"}, nil
}

// Done asks the operator to confirm they typed into the focused field.
// "Stop" is first (the only choice that could carry a value); anything but
// Done — including a timeout — is false.
func (Asker) Done(ctx context.Context, q guard.GuardAsk) (bool, error) {
	site := q.Site
	if site == "" {
		site = "the page"
	}
	res, err := form(ctx, ask.FormSpec{
		Title: "bashy browser guard",
		Prompt: fmt.Sprintf("The field for %q on %s is focused in your browser. Type your answer THERE, then choose %q. "+
			"Do not type it in this box.", q.Question, site, ChoiceDone),
		Field:   ask.FieldSpec{Name: "Leave this empty", Kind: "text"},
		Choices: []string{ChoiceStop, ChoiceDone},
		Timeout: q.Timeout,
	})
	if err != nil {
		return false, nil
	}
	discard(res.ID)
	return res.Choice == ChoiceDone, nil
}

// discard consumes a value nobody asked for, so it does not sit in the sink.
func discard(id string) {
	if id != "" {
		if v, err := take(id); err == nil {
			clear(v)
		}
	}
}

// Prompt is the operator's question: the site, the question text and why it
// was blocked. It never carries the refused value.
func Prompt(q guard.GuardAsk) string {
	site := q.Site
	if site == "" {
		site = "(unknown site)"
	}
	question := q.Question
	if question == "" {
		question = "(no visible question)"
	}
	var opts []string
	if q.CanProvide {
		opts = append(opts, fmt.Sprintf("%q or %q: bashy fills the field for you; the agent never sees it", ChoiceEnter, ChoiceClipboard))
	}
	if q.CanSelf {
		opts = append(opts, fmt.Sprintf("%q: bashy focuses the field in your browser", ChoiceSelf))
	}
	opts = append(opts, fmt.Sprintf("%q: leave this question", ChoiceSkip),
		fmt.Sprintf("%q: stop filling this form (also if nobody answers)", ChoiceStop))
	return fmt.Sprintf("An agent tried to %s on %s, question %q. Blocked: %s (%s).\n%s.",
		strings.ReplaceAll(q.Action, "_", " "), site, question, q.Why, q.Reason, strings.Join(opts, "; "))
}

// FieldName labels the form's entry so pkg/sensitive classifies the answer
// (and the clipboard) as the right kind.
func FieldName(category string) string {
	names := map[string]string{
		"ssn": "Social Security number", "itin": "ITIN", "ein": "EIN", "routing": "Routing number",
		"iban": "IBAN", "card": "Card number", "account": "Account number", "otp": "One-time code",
		"password": "Password", "username": "Username", "secret": "Secret",
	}
	if n := names[category]; n != "" {
		return n
	}
	return "Answer"
}
