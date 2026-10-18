// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package kb_test

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
)

var senseNow = time.Date(2026, 9, 13, 8, 15, 0, 0, time.UTC)

func senseReading() kb.SenseReading {
	return kb.SenseReading{
		SchemaVersion: kb.SenseSchemaVersion,
		SenseID:       "cpu-0",
		Subject:       "host:buildbox",
		Source:        "sense:cpu-0",
		ObservedAt:    "2026-09-13T08:14:00Z",
		Cadence:       "60s",
		Status:        "ok",
		Value:         []byte(`{"load":0.4}`),
		Snapshot:      "sha256:abc123",
	}
}

func TestSenseFreshness(t *testing.T) {
	rd := senseReading()
	for _, tt := range []struct {
		name string
		mut  func(*kb.SenseReading)
		now  time.Time
		want string
	}{
		{"fresh", func(r *kb.SenseReading) {}, senseNow, "fresh"},
		{"fresh at the cadence edge", func(r *kb.SenseReading) {}, rdTime(rd, 60), "fresh"},
		{"stale past cadence", func(r *kb.SenseReading) {}, rdTime(rd, 61), "stale"},
		{"unavailable status", func(r *kb.SenseReading) { r.Status = "unavailable" }, senseNow, "unknown"},
		{"unknown status", func(r *kb.SenseReading) { r.Status = "unknown" }, senseNow, "unknown"},
		{"error status", func(r *kb.SenseReading) { r.Status = "error" }, senseNow, "unknown"},
		{"missing observed_at", func(r *kb.SenseReading) { r.ObservedAt = "" }, senseNow, "unknown"},
		{"invalid observed_at", func(r *kb.SenseReading) { r.ObservedAt = "yesterday" }, senseNow, "unknown"},
		{"missing cadence", func(r *kb.SenseReading) { r.Cadence = "" }, senseNow, "unknown"},
		{"invalid cadence", func(r *kb.SenseReading) { r.Cadence = "often" }, senseNow, "unknown"},
		{"observed_at in the future", func(r *kb.SenseReading) {}, senseNow.Add(-2 * time.Minute), "unknown"},
	} {
		rd := senseReading()
		tt.mut(&rd)
		if got := kb.SenseFreshness(rd, tt.now); got != tt.want {
			t.Errorf("%s: freshness = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// rdTime returns observed_at + secs.
func rdTime(rd kb.SenseReading, secs int) time.Time {
	at, _ := time.Parse(time.RFC3339, rd.ObservedAt)
	return at.Add(time.Duration(secs) * time.Second)
}

func TestSenseOutcomeExitCodes(t *testing.T) {
	for outcome, want := range map[kb.Outcome]int{
		kb.OutcomeFound: 0, kb.OutcomeRedacted: 0, kb.OutcomeExpired: 0,
		kb.OutcomeNotFound: 1, kb.OutcomeDenied: 1, kb.OutcomeError: 1,
	} {
		if got := outcome.ExitCode(); got != want {
			t.Errorf("outcome %q exit = %d, want %d", outcome, got, want)
		}
	}
}
