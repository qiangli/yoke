package cligw

// Sprint: #290 (W1: claude:opus5 answered tool calls with a sentence first)

import "testing"

func TestToolEnvelopeTail(t *testing.T) {
	env := `{"tool_calls":[{"name":"bash","arguments":{"command":"ls /testbed"}}]}`
	for in, want := range map[string]string{
		env: env,
		"I'll start by exploring the environment.\n\n" + env:    env,
		"Let me look.\n```json\n" + env + "\n```":               env,
		"Plain answer, no tool call.":                           "Plain answer, no tool call.",
		`The format is {"tool_calls": []} as documented.`:       `The format is {"tool_calls": []} as documented.`,
		"Prose then broken {\"tool_calls\":[{\"name\":\"bash\"": "Prose then broken {\"tool_calls\":[{\"name\":\"bash\"",
	} {
		if got := toolEnvelopeTail(in); got != want {
			t.Errorf("toolEnvelopeTail(%q) = %q, want %q", in, got, want)
		}
	}
}
