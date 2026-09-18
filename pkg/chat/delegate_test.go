// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package chat

import (
	"io"
	"testing"
)

// delegate must name a target before it spawns anything — a bare `delegate` is a
// usage error, not a launch of some default agent.
func TestDelegateRequiresATarget(t *testing.T) {
	cmd := NewDelegateCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error when no agent/self/instruction is given")
	}
}

// A named target with no instruction is also an error (don't launch an agent with
// nothing to do).
func TestDelegateRequiresAnInstruction(t *testing.T) {
	cmd := NewDelegateCmd()
	cmd.SetArgs([]string{"codex"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error when a target has no instruction")
	}
}
