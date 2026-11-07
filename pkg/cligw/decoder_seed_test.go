package cligw

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func TestSeededDecoderAndStdinShapes(t *testing.T) {
	fleettest.Ring(t)
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutLocalStore())
	for _, tc := range []struct{ name, line, text, format string }{
		{"agy", `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hello"}}`, "hello", "agy-steps"},
		{"muse", `{"payload_type":"run.output.delta","payload":{"text":"hello"}}`, "hello", ""},
		{"claude", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`, "hello", "claude-stream-json"},
		{"codex", `{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}`, "hello", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := c.Tool(tc.name)
			if !ok {
				t.Fatal("missing tool")
			}
			ev, ok := parseEvent([]byte(tc.line), tool.CLI.Launch.EventsDone, tool.CLI.Launch.Decoder)
			if !ok || ev.Text != tc.text {
				t.Fatalf("event = %+v, parsed=%v", ev, ok)
			}
			if tool.CLI.Launch.StdinFormat != tc.format {
				t.Fatalf("stdin format = %q", tool.CLI.Launch.StdinFormat)
			}
		})
	}
}
