package ask

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ctty"
)

func TestFormChoicesAndTakeOnce(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	old := formDialog
	defer func() { formDialog = old }()
	choices := []string{"Enter it here", "I'll type it in the page myself", "Skip this question", "Stop"}
	for _, kind := range []string{"text", "secret"} {
		formDialog = func(_ context.Context, r ctty.FormRequest) (ctty.FormAnswer, error) {
			if r.Kind != kind || len(r.Choices) != 5 || r.Choices[1] != "Use clipboard" {
				t.Fatalf("request: %+v", r)
			}
			return ctty.FormAnswer{Choice: choices[0], Value: "sensitive-value"}, nil
		}
		got, err := Form(context.Background(), FormSpec{Field: FieldSpec{Name: "ID", Kind: kind}, Choices: choices})
		if err != nil || got.Choice != choices[0] || got.ID == "" {
			t.Fatalf("result=%+v err=%v", got, err)
		}
		b, err := TakeValue(got.ID)
		if err != nil || string(b) != "sensitive-value" {
			t.Fatalf("take err=%v", err)
		}
		if _, err := TakeValue(got.ID); err == nil {
			t.Fatal("second take succeeded")
		}
		if _, err := os.Stat(requestDir(got.ID)); !os.IsNotExist(err) {
			t.Fatalf("request directory remained: %v", err)
		}
	}
	for _, choice := range choices[1:] {
		formDialog = func(context.Context, ctty.FormRequest) (ctty.FormAnswer, error) {
			return ctty.FormAnswer{Choice: choice, Value: "ignored"}, nil
		}
		got, err := Form(context.Background(), FormSpec{Field: FieldSpec{Kind: "text"}, Choices: choices})
		if err != nil || got.Choice != choice || got.ID != "" {
			t.Fatalf("result=%+v err=%v", got, err)
		}
	}
	formDialog = func(context.Context, ctty.FormRequest) (ctty.FormAnswer, error) {
		return ctty.FormAnswer{Choice: "Use clipboard", Value: "123-45-6789"}, nil
	}
	got, err := Form(context.Background(), FormSpec{Field: FieldSpec{Name: "SSN", Kind: "text"}, Choices: choices})
	if err != nil || got.Choice != "Use clipboard" || got.ID == "" {
		t.Fatalf("clipboard result=%+v err=%v", got, err)
	}
	if b, err := TakeValue(got.ID); err != nil || string(b) != "123-45-6789" {
		t.Fatalf("clipboard take err=%v", err)
	}
	formDialog = func(context.Context, ctty.FormRequest) (ctty.FormAnswer, error) {
		return ctty.FormAnswer{Choice: "Use clipboard", Value: "not-an-ssn"}, nil
	}
	if _, err := Form(context.Background(), FormSpec{Field: FieldSpec{Name: "SSN", Kind: "text"}, Choices: choices}); err == nil || strings.Contains(err.Error(), "not-an-ssn") {
		t.Fatalf("shape mismatch leaked or accepted: %v", err)
	}
}

func TestFormTimeoutAndInvalidKind(t *testing.T) {
	old := formDialog
	defer func() { formDialog = old }()
	formDialog = func(context.Context, ctty.FormRequest) (ctty.FormAnswer, error) {
		return ctty.FormAnswer{}, ctty.ErrTimeout
	}
	if got, err := Form(context.Background(), FormSpec{Field: FieldSpec{Kind: "secret"}, Choices: []string{"Enter it here", "Stop"}}); err != nil || got.Choice != "Stop" {
		t.Fatalf("timeout=%+v err=%v", got, err)
	}
	if _, err := Form(context.Background(), FormSpec{Field: FieldSpec{Kind: "bad"}, Choices: []string{"Stop"}}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	if _, err := TakeValue("../outside"); err == nil {
		t.Fatal("path-like ID accepted")
	}
}
