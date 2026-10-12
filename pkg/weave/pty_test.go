package weave

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/chat"
)

func TestWeaveCoachSinkAcceptsNilLogSink(t *testing.T) {
	var coach bytes.Buffer
	sink := weaveCoachSink(nil, &coach)
	if _, err := sink.Write([]byte("event\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := coach.String(), "event\n"; got != want {
		t.Fatalf("coach output = %q, want %q", got, want)
	}
}

// Sprint 423 run 152: a productive Muse worker streamed ~1100 MSP envelopes, the
// coach read each unique id as new content and each ~1KB envelope as ~250 output
// tokens, and raised UNRESOLVED LOOP three times. Drive the same path weave drives
// (stream-json writer -> sink -> coach) and require silence.
func TestWeaveCoachQuietOnMuseEnvelopeFlood(t *testing.T) {
	var log bytes.Buffer
	pol := chat.DefaultCoachPolicy()
	coach := chat.NewLineCoach(pol, chat.NewCtlSteerer(""))
	sj := newWeaveStreamJSONLogWriter(weaveCoachSink(&log, coach))
	for i := 0; i < 1200; i++ {
		id := fmt.Sprintf("00000000-0000-7000-8000-%012d", i)
		line := fmt.Sprintf(`{"schema_version":1,"id":%q,"stream":{"kind":"session","id":"s"},"sequence":%d,"recorded_at":%d,"record_type":"event","payload_type":"task.lifecycle.status","payload":{"kind":"task_lifecycle","task_id":%q,"event":{"kind":"status","message":%q}}}`+"\n",
			id, i, 1780531400000000+i, id, strings.Repeat("progress detail ", 40))
		if _, err := sj.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if coach.Unresolved() || len(coach.Report().Steers) != 0 {
		t.Fatalf("coach steered a run that only produced distinct envelopes: %+v", coach.Report().Steers)
	}
	if got := coach.OutputTokens(); got != 0 {
		t.Fatalf("envelope bytes were counted as output tokens: %d", got)
	}
	if log.Len() == 0 {
		t.Fatal("the worker log must still receive the stream")
	}
}
