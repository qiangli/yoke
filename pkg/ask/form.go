package ask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/qiangli/yoke/pkg/ctty"
	"github.com/qiangli/yoke/pkg/sensitive"
)

type FieldSpec struct{ Name, Kind string }
type FormSpec struct {
	Title, Prompt string
	Field         FieldSpec
	Choices       []string
	Timeout       time.Duration
}
type FormResult struct{ Choice, ID string }

var formDialog = ctty.FormGUI
var askIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Form obtains one operator choice. Only the first choice may carry a value;
// that value is written to a private 0600 sink and never returned to the caller.
func Form(ctx context.Context, spec FormSpec) (FormResult, error) {
	if spec.Field.Kind != "text" && spec.Field.Kind != "secret" {
		return FormResult{}, fmt.Errorf("ask: invalid field kind %q", spec.Field.Kind)
	}
	if len(spec.Choices) < 2 {
		return FormResult{}, errors.New("ask: form needs choices")
	}
	if spec.Timeout <= 0 {
		spec.Timeout = 2 * time.Minute
	}
	spec.Choices = withClipboardChoice(spec.Choices)
	if ctx.Err() != nil {
		return FormResult{Choice: stopChoice(spec.Choices)}, nil
	}
	category := sensitive.ClipboardCategory(spec.Field.Name+" "+spec.Prompt, spec.Field.Kind)
	answer, err := formDialog(ctx, ctty.FormRequest{Title: spec.Title, Prompt: sanitizePrompt(spec.Prompt), Name: spec.Field.Name, Kind: spec.Field.Kind, Choices: spec.Choices, Timeout: spec.Timeout, ClipboardCategory: category})
	if err != nil {
		return FormResult{Choice: stopChoice(spec.Choices)}, nil
	}
	result := FormResult{Choice: answer.Choice}
	if answer.Choice != spec.Choices[0] && answer.Choice != "Use clipboard" {
		return result, nil
	}
	if answer.Choice == "Use clipboard" {
		if valid, reason := sensitive.ClipboardShape(category, answer.Value); !valid {
			return FormResult{}, fmt.Errorf("ask: %s", reason)
		}
	}
	if answer.Value == "" {
		return FormResult{}, ctty.ErrDeclined
	}
	id, err := newID()
	if err != nil {
		return FormResult{}, err
	}
	now := time.Now()
	r := Request{SchemaVersion: SchemaVersion, ID: id, Prompt: sanitizePrompt(spec.Prompt), Name: spec.Field.Name,
		Secret: spec.Field.Kind == "secret", Created: now, Expires: now.Add(spec.Timeout), ValueExpires: now.Add(15 * time.Minute),
		Sink: Sink{Kind: SinkFile, Detail: "private one-shot value"}, Requester: currentRequester(), Kind: "form"}
	if err := save(r); err != nil {
		return FormResult{}, err
	}
	if err := writeSecretFile(filepath.Join(requestDir(id), valueFile), []byte(answer.Value)); err != nil {
		_ = os.RemoveAll(requestDir(id))
		return FormResult{}, err
	}
	result.ID = id
	return result, nil
}

func withClipboardChoice(choices []string) []string {
	if len(choices) < 2 || choices[0] != "Enter it here" || choices[len(choices)-1] != "Stop" {
		return choices
	}
	for _, choice := range choices {
		if choice == "Use clipboard" {
			return choices
		}
	}
	out := make([]string, 0, len(choices)+1)
	out = append(out, choices[0], "Use clipboard")
	out = append(out, choices[1:]...)
	return out
}

// TakeValue consumes a form value exactly once. Callers must pass the opaque ID,
// never a path. A successful read removes the entire request directory.
func TakeValue(id string) ([]byte, error) {
	if !askIDPattern.MatchString(id) {
		return nil, errors.New("ask: invalid id")
	}
	r, err := Load(id)
	if err != nil {
		return nil, err
	}
	if r.Kind != "form" || time.Now().After(r.ValueExpires) {
		return nil, errors.New("ask: no live form value")
	}
	path := filepath.Join(requestDir(id), valueFile)
	claimed := filepath.Join(requestDir(id), "value.claimed")
	if err := os.Rename(path, claimed); err != nil {
		return nil, errors.New("ask: value already consumed")
	}
	defer os.RemoveAll(requestDir(id))
	return os.ReadFile(claimed)
}

func stopChoice(choices []string) string {
	for _, choice := range choices {
		if choice == "Stop" {
			return choice
		}
	}
	return ""
}
