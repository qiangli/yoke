package chat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// The measured opencode 1.18.30 failure: the error event on stdout, exit 0.
const measuredOpencodeFailure = `{"type":"error","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details."}}}`

// TestFailEventHelperProcess is the child a test below re-execs: it plays an
// agent CLI that prints a stream and exits 0 (BASHY_TEST_STREAM selects it).
func TestFailEventHelperProcess(t *testing.T) {
	stream := os.Getenv("BASHY_TEST_STREAM")
	if stream == "" {
		return
	}
	switch stream {
	case "opencode-error":
		fmt.Println(`{"type":"step_start","timestamp":1}`)
		fmt.Println(measuredOpencodeFailure)
	case "ok":
		fmt.Println(`{"type":"text","part":{"text":"SMOKE-OK"}}`)
		fmt.Println(`{"type":"step_finish","part":{"reason":"stop"}}`)
	}
	os.Exit(0)
}

func runFailEventChild(t *testing.T, stream string, l Launch) (string, int, error) {
	t.Helper()
	t.Setenv("BASHY_TEST_STREAM", stream)
	t.Setenv("BASHY_NO_COACH", "1")
	ctx := withLaunch(context.Background(), l)
	return execRunner{}.Run(ctx, os.Args[0], []string{"-test.run=^TestFailEventHelperProcess$"}, t.TempDir())
}

// RED→GREEN for story f6292e59c914's second defect: a tool whose recipe
// declares events_fail and which prints that event while exiting 0 is
// reported as a non-zero exit with the line; the same tool on a healthy
// stream, and a tool with no declaration, keep their exit 0.
func TestExecRunnerTurnsDeclaredFailureEventIntoNonZeroExit(t *testing.T) {
	opencode := Launch{ToolName: "opencode", FailEvents: fleet.EventsDone{Field: "type", Values: []string{"error"}}}

	out, code, err := runFailEventChild(t, "opencode-error", opencode)
	if code != ExitToolFailure || err == nil {
		t.Fatalf("declared failure event: code=%d err=%v out=%q", code, err, out)
	}
	if !strings.Contains(err.Error(), "opencode reported a failure event and exited 0") || !strings.Contains(err.Error(), "UnknownError") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(out, measuredOpencodeFailure) {
		t.Fatalf("captured output lost the stream: %q", out)
	}

	if _, code, err := runFailEventChild(t, "ok", opencode); code != 0 || err != nil {
		t.Fatalf("healthy stream: code=%d err=%v", code, err)
	}
	undeclared := Launch{ToolName: "opencode"}
	if _, code, err := runFailEventChild(t, "opencode-error", undeclared); code != 0 || err != nil {
		t.Fatalf("undeclared recipe must keep the tool's exit: code=%d err=%v", code, err)
	}
}
