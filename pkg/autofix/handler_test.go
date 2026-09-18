package autofix

import (
	"context"
	"reflect"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

func TestHandlerRequiresExplicitAgenticMode(t *testing.T) {
	t.Setenv("BASHY_HINTS", "on")
	var got []string
	next := interp.ExecHandlerFunc(func(_ context.Context, args []string) error {
		got = append([]string(nil), args...)
		return nil
	})
	h := Handler()(next)

	t.Setenv("BASHY_AGENTIC", "0")
	input := []string{"sed", "-r", "s/x/y/"}
	if err := h(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("agentic off rewrote argv: %v", got)
	}
}
