package foreman

import (
	"context"
	"strings"
	"testing"
)

type captureInstructionRunner struct{ instructions []string }

func (r *captureInstructionRunner) Run(_ context.Context, _ string, args []string, _ string) (string, int, error) {
	if len(args) > 0 {
		r.instructions = append(r.instructions, args[len(args)-1])
	}
	return "", 0, nil
}

func TestFirstTellAppearsOnceInOpeningPrompt(t *testing.T) {
	runner := &captureInstructionRunner{}
	s, err := Start(context.Background(), Options{
		ID: "instruction-once", Goal: "manage the sprint", Agent: "stub", Root: t.TempDir(), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	exact := "EXACT $HOME prompt; once"
	if err := s.Apply(context.Background(), Command{Verb: CommandTell, Message: exact}); err != nil {
		t.Fatal(err)
	}
	if len(runner.instructions) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.instructions))
	}
	if got := strings.Count(runner.instructions[0], exact); got != 1 {
		t.Fatalf("instruction occurs %d times, want 1:\n%s", got, runner.instructions[0])
	}
}
