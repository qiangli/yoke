//go:build !aix

package telemetry

import (
	"context"
	"errors"
	"mvdan.cc/sh/v3/interp"
	"testing"
	"time"
)

func TestServiceStatusHandshakePreservesTypedErrorsAndRecovery(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	at := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	state := StatusHealthy
	handler := ExecMiddleware(func(ctx context.Context, args []string) error {
		ServiceStatus(ctx, obs("review-service", "review-target", state, "", at))
		MarkServiceStatusReported(ctx)
		if state == StatusError {
			return interp.ExitStatus(3)
		}
		return nil
	})
	argv := []string{"service", "status", "--json"}
	if err := handler(context.Background(), argv); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Second)
	if err := handler(context.Background(), argv); err != nil {
		t.Fatal(err)
	}
	state = StatusError
	at = at.Add(time.Second)
	if err := handler(context.Background(), argv); !errors.Is(err, interp.ExitStatus(3)) {
		t.Fatalf("lost unhealthy command exit: %v", err)
	}
	state = StatusHealthy
	at = at.Add(time.Second)
	if err := handler(context.Background(), argv); err != nil {
		t.Fatal(err)
	}
	records := statusAttrs(t, sr)
	if len(records) != 3 {
		t.Fatalf("expected first,error,recovery only: %v", records)
	}
	for _, record := range records {
		if record["_name"] != "service.status" {
			t.Fatalf("generic poll span survived typed handshake: %v", record)
		}
	}
	if records[1]["status.state"] != StatusError || records[2]["status.recovered"] != "true" {
		t.Fatalf("lost transition facts: %v", records)
	}
}

func TestServiceStatusLookalikeWithoutHandshakeKeepsFailure(t *testing.T) {
	sr := withRecorder(t)
	handler := ExecMiddleware(func(context.Context, []string) error { return interp.ExitStatus(7) })
	err := handler(context.Background(), []string{"service", "status", "--json"})
	if !errors.Is(err, interp.ExitStatus(7)) {
		t.Fatalf("changed result: %v", err)
	}
	a := attrs(t, sr)
	if a["cmd.name"] != "service" || a["cmd.exit_code"] != "7" {
		t.Fatalf("untyped command lost evidence: %v", a)
	}
}

func TestServiceStatusHandshakeKeepsQueryFailure(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	want := errors.New("injected query failure")
	handler := ExecMiddleware(func(ctx context.Context, _ []string) error {
		ServiceStatusQueryFailed(ctx, StatusQueryFailure{Service: "query-service", Target: "query-target", Err: want.Error()})
		MarkServiceStatusReported(ctx)
		return want
	})
	if err := handler(context.Background(), []string{"service", "status", "--json"}); err != want {
		t.Fatalf("changed query result: %v", err)
	}
	records := statusAttrs(t, sr)
	if len(records) != 1 || records[0]["_name"] != "service.status_query_failed" || records[0]["status.query_error"] != want.Error() {
		t.Fatalf("lost query failure: %v", records)
	}
}
