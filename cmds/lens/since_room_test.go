package lenscmd

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestAgentWorldBusTimelineSince(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	if err := room.Emit(room.Event{Type: room.EventNotify, Topic: "quota", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := runTool(t, t.TempDir(), "", "--since=start", "-t", "jsonl", "bus:timeline")
	if code != 0 || stderr != "" {
		t.Fatalf("first %d %q", code, stderr)
	}
	rows, cursor := deltaCursor(t, out)
	if !strings.Contains(rows, `"body":"one"`) {
		t.Fatalf("first rows %q", rows)
	}
	out, stderr, code = runTool(t, t.TempDir(), "", "--since", cursor, "-t", "jsonl", "bus:timeline")
	if code != 0 || stderr != "" {
		t.Fatalf("idle %d %q", code, stderr)
	}
	rows, cursor = deltaCursor(t, out)
	if rows != "" {
		t.Fatalf("idle rows %q", rows)
	}
	if err := room.Emit(room.Event{Type: room.EventNotify, Topic: "quota", Body: "two"}); err != nil {
		t.Fatal(err)
	}
	out, stderr, code = runTool(t, t.TempDir(), "", "--since", cursor, "-t", "jsonl", "bus:timeline")
	if code != 0 || stderr != "" {
		t.Fatalf("append %d %q", code, stderr)
	}
	rows, _ = deltaCursor(t, out)
	if !strings.Contains(rows, `"body":"two"`) || strings.Contains(rows, `"body":"one"`) {
		t.Fatalf("append rows %q", rows)
	}
}
