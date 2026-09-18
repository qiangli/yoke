// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// These tests cover the FAILED path of a RunRecord, which the original
// privacy test could not reach: it exercised a passing record, whose Failure is
// nil by construction, so neither the leak nor the misclassification below was
// observable through it. NeverPublishesRawErrorText,
// UnreachableWorkerIsNeverAConformanceVerdict, and the producer-carrier test
// discriminate against the pre-fix contract. The ran-body half of
// TransportMustClassifyItsOwnUndeliverability and
// CancellationIsInfraNotAVerdict are regression pins; they already passed
// there.

// unreachableErr is the shape a transport produces when it cannot dial its
// worker: the address is in the error text, because that is what makes the
// error useful to the operator reading a log.
func unreachableErr(addr string) error {
	return fmt.Errorf("dial tcp %s: connect: connection refused: %w", addr, ErrWorkerUnreachable)
}

// TestRunRecordNeverPublishesRawErrorText is the failed-path privacy gate. A
// record is a committed artifact, so no part of a raw transport error may
// survive into it — not the address, not the port, not the operator-facing
// text that carried them.
func TestRunRecordNeverPublishesRawErrorText(t *testing.T) {
	const addr = "203.0.113.7:22"

	rec := RecordAttempt(
		&Task{Name: "suite:shard=2"},
		&Worker{ID: "arm-builder", Host: "203.0.113.7", Venues: []string{VenueUserland}},
		1,
		TaskResult{Status: StatusFailed, ExitCode: 255, Err: unreachableErr(addr)},
	)

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"203.0.113.7", ":22", "dial tcp", "connection refused"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("serialized run record carries raw error text %q:\n%s", forbidden, data)
		}
	}
	if err := rec.Validate(); err != nil {
		t.Errorf("Validate rejected a record the recorder produced: %v", err)
	}
}

// TestUnreachableWorkerIsNeverAConformanceVerdict is the acceptance gate in
// test form: an unreachable worker judged nothing, so it must not produce a
// verdict against the code under test.
func TestUnreachableWorkerIsNeverAConformanceVerdict(t *testing.T) {
	rec := RecordAttempt(
		&Task{Name: "suite:shard=2"},
		&Worker{ID: "arm-builder", Venues: []string{VenueUserland}},
		1,
		TaskResult{Status: StatusFailed, ExitCode: 255, Err: unreachableErr("203.0.113.7:22")},
	)

	if rec.Status != RunInfraFailed {
		t.Errorf("unreachable worker recorded status %q, want %q — an unreachable worker rendered no verdict",
			rec.Status, RunInfraFailed)
	}
	if rec.Status.HasVerdict() {
		t.Error("HasVerdict() is true for an unreachable attempt; infra failures are void attempts, not verdicts")
	}
	if rec.Failure == nil || rec.Failure.Code != FailUnreachable {
		t.Errorf("failure code = %+v, want %q", rec.Failure, FailUnreachable)
	}
}

// TestTransportMustClassifyItsOwnUndeliverability documents where the burden
// of "a verdict must be positively earned" actually sits, and why it cannot sit
// in RecordAttempt.
//
// A conformance failure and an unclassified transport failure arrive here in
// the SAME shape: StatusFailed with a generic error. So a recorder that tried
// to be cautious — treating every unrecognised error as infra — would silently
// stop reporting real conformance failures, which is the more expensive error
// of the two in a conformance campaign. The burden therefore belongs upstream:
// a transport that cannot deliver a task MUST mark it, and the sentinels are
// what keep an undeliverable attempt from ever reaching the verdict branch.
//
// This test pins both halves of that contract, so a future transport author
// who forgets to wrap ErrWorkerUnreachable sees the obligation stated here.
func TestTransportMustClassifyItsOwnUndeliverability(t *testing.T) {
	// A body that ran and lost is a real verdict and must stay one.
	ran := RecordAttempt(&Task{Name: "t"}, nil, 1,
		TaskResult{Status: StatusFailed, ExitCode: 1, Err: errors.New("exit 1")})
	if ran.Status != RunFailed || !ran.Status.HasVerdict() {
		t.Errorf("a body that ran and failed recorded %q (verdict=%v), want %q with a verdict",
			ran.Status, ran.Status.HasVerdict(), RunFailed)
	}

	// The SAME shape, marked by the transport as undeliverable, must not be.
	marked := RecordAttempt(&Task{Name: "t"}, nil, 1,
		TaskResult{Status: StatusFailed, ExitCode: 1, Err: unreachableErr("203.0.113.7:22")})
	if marked.Status != RunInfraFailed || marked.Status.HasVerdict() {
		t.Errorf("an undeliverable attempt recorded %q (verdict=%v), want %q with no verdict",
			marked.Status, marked.Status.HasVerdict(), RunInfraFailed)
	}
}

// TestCancellationIsInfraNotAVerdict guards the other no-verdict path.
func TestCancellationIsInfraNotAVerdict(t *testing.T) {
	rec := RecordAttempt(&Task{Name: "t"}, nil, 1, TaskResult{
		Status: StatusFailed,
		Err:    fmt.Errorf("stopped: %w", context.Canceled),
	})
	if rec.Status != RunInfraFailed || rec.Status.HasVerdict() {
		t.Errorf("cancelled attempt recorded %q (verdict=%v), want %q with no verdict",
			rec.Status, rec.Status.HasVerdict(), RunInfraFailed)
	}
	if rec.Failure == nil || rec.Failure.Code != FailCanceled {
		t.Errorf("failure code = %+v, want %q", rec.Failure, FailCanceled)
	}
}

// proseCarrier has useful operator-facing prose but can publish only its stable
// classification to a RunRecord.
type proseCarrier struct {
	text string
	code string
}

func (e proseCarrier) Error() string { return e.text }
func (e proseCarrier) FleetFailure() (RunStatus, FailureReason) {
	return RunFailed, FailureReason{Code: e.code}
}

// TestProducerProseCannotReachRecord covers both directions that made a
// content heuristic unsafe: legitimate conformance prose must retain its
// verdict, while reach-bearing prose must be absent from serialized records.
func TestProducerProseCannotReachRecord(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		legitimate bool
	}{
		{"task-name", "task suite:shard=2: checksum mismatch", true},
		{"module-pin", "module mvdan.cc/sh@v3.13.1: vet failed", true},
		{"time", "timeout at 09:56", true},
		{"ratio", "coverage ratio 3:2", true},
		{"dotted-version", "expected 1.2.3.4 got 1.2.3.5", true},
		{"hostname", "ssh internal-builder-3 refused", false},
		{"url", "Get https://203.0.113.7:8443/api: connection refused", false},
		{"ipv6-zone", "fe80::1%eth0", false},
		{"ipv6-loopback", "::1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := RecordAttempt(&Task{Name: "t"}, nil, 1, TaskResult{
				Status: StatusFailed,
				Err:    proseCarrier{text: tc.text, code: FailExitNonzero},
			})
			if rec.Status != RunFailed || rec.Failure == nil || rec.Failure.Code != FailExitNonzero {
				t.Fatalf("record = %q/%+v, want %q/%q", rec.Status, rec.Failure, RunFailed, FailExitNonzero)
			}
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), tc.text) {
				t.Fatalf("producer prose reached serialized record: %s", data)
			}
			if tc.legitimate && !rec.Status.HasVerdict() {
				t.Fatal("legitimate conformance detail was treated as a leak and voided")
			}
		})
	}
}

func TestProducerUnknownCodeIsDowngraded(t *testing.T) {
	rec := RecordAttempt(&Task{Name: "t"}, nil, 1, TaskResult{
		Status: StatusFailed,
		Err:    proseCarrier{text: "operator detail", code: "unknown-code"},
	})
	if rec.Status != RunInfraFailed || rec.Failure == nil || rec.Failure.Code != FailUnclassified {
		t.Fatalf("unknown carrier code produced %q/%+v, want %q/%q",
			rec.Status, rec.Failure, RunInfraFailed, FailUnclassified)
	}
}
