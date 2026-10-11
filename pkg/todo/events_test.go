package todo

import (
	"bytes"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestTodoStatusCommandsEmitHostEvents(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	base := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := NewTodoCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--base-dir", base}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("todo %v: %v\n%s", args, err, out.String())
		}
	}
	run("add", "ship events")
	run("start", "1")
	run("done", "1")
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("non-contiguous seq: %+v", events)
		}
		if e.Type == room.EventTodo {
			got = append(got, e.Topic)
		}
	}
	if len(got) != 2 || got[0] != StatusDoing || got[1] != StatusDone {
		t.Fatalf("todo events = %v, want [doing done]", got)
	}
}
