package weave

import (
	"bytes"
	"testing"
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
