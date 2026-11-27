package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDecisionLogRoundTrip(t *testing.T) {
	home := t.TempDir()
	legacy := AutoDecision{Time: time.Now().UTC(), Event: "trigger", Result: "wish"}
	full := AutoDecision{Time: legacy.Time, Event: "undo", Result: "activated", Rev: "r1", Base: "r0", Principal: "user", Line: "owner", Agent: "binding", Problem: "todo:1", Why: "reason", Expect: "prediction", By: "person"}
	raw, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(raw), "principal") || strings.Contains(string(raw), "expect") {
		t.Fatalf("legacy record: %s %v", raw, err)
	}
	for _, d := range []AutoDecision{legacy, full} {
		if err := LogAuto(home, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadAutoLog(home)
	if err != nil || len(got) != 2 || got[0] != legacy || got[1] != full {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
