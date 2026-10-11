//go:build !windows

package bus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// This is the native process gate for tool-cmd. The stub stands in for the
// mounted bashy front door and records both the tool command argv and its stdin.
func TestToolCmdSinkCallsStubCommand(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	stub := filepath.Join(dir, "bashy-stub")
	argvCapture := filepath.Join(dir, "argv")
	stdinCapture := filepath.Join(dir, "stdin")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$BASHY_TEST_TOOL_ARGV\"\n" +
		"cat > \"$BASHY_TEST_TOOL_STDIN\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_BIN", stub)
	t.Setenv("BASHY_TEST_TOOL_ARGV", argvCapture)
	t.Setenv("BASHY_TEST_TOOL_STDIN", stdinCapture)
	subscribe(t, Subscription{
		Subscriber: "human", Kinds: []string{room.EventApproval},
		Sink: "tool-cmd", SinkTarget: "stub:send",
	})
	if err := room.Emit(room.Event{
		Type: room.EventApproval, Actor: "operator", Target: "approval:9",
		Topic: "requested", Body: "approve deploy\nlocal-only detail",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := testSidecar(t, &recorder{}).Once()
	if err != nil {
		t.Fatal(err)
	}
	if res.Sinks != 1 || res.Demoted != 0 {
		t.Fatalf("result = %+v, want one successful sink", res)
	}
	argv, err := os.ReadFile(argvCapture)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := "tool\ncmd\nrun\nstub:send\n--\napprove deploy\n"
	if string(argv) != wantArgv {
		t.Fatalf("stub argv:\n%s\nwant:\n%s", argv, wantArgv)
	}
	stdin, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatal(err)
	}
	var event EventsLine
	if err := json.Unmarshal(stdin, &event); err != nil {
		t.Fatalf("stub stdin is not event JSON: %v\n%s", err, stdin)
	}
	if event.Type != room.EventApproval || event.Body != "approve deploy" || strings.Contains(string(stdin), "local-only") {
		t.Fatalf("stub stdin leaked more than the subject: %+v\n%s", event, stdin)
	}
}
