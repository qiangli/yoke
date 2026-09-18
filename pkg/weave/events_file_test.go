package weave

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFollowWeaveEventFileReportsIncrementalActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var log bytes.Buffer
	activity, stop := followWeaveEventFile(path, &log)

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"turn.start\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-activity:
	case <-time.After(time.Second):
		t.Fatal("event file activity was not reported")
	}
	stop()
	if got := log.String(); !strings.Contains(got, "[event] turn started") {
		t.Fatalf("log = %q", got)
	}
}

func TestDistillWeaveEventFileLine(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"turn.start"}`:                            "[event] turn started",
		`{"type":"tool.call","data":{"name":"read_file"}}`: "[event] -> read_file",
		`{"type":"turn.end","data":{"status":"ok"}}`:       "[event] turn ended status=ok",
	} {
		if got := weaveDistillEventFileLine([]byte(in)); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}
