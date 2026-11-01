// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package appserver

import (
	"testing"
)

// TestParseRepair: shaped.json's repair key caps self-repair attempts per
// incident — a non-negative number, or "unlimited". Missing (or null) means
// the console default. A present but invalid value does NOT fail the
// manifest and never means unlimited: the console logs a warning and uses
// its default.
func TestParseRepair(t *testing.T) {
	m, err := Parse([]byte(`{"repair": {"max_attempts": 3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Repair == nil || !m.Repair.MaxAttempts.Set || !m.Repair.MaxAttempts.Valid ||
		m.Repair.MaxAttempts.Unlimited || m.Repair.MaxAttempts.N != 3 {
		t.Fatalf("repair 3 = %+v, %v", m.Repair, err)
	}
	m, err = Parse([]byte(`{"repair": {"max_attempts": 0}}`))
	if err != nil || m.Repair == nil || !m.Repair.MaxAttempts.Valid || m.Repair.MaxAttempts.N != 0 {
		t.Fatalf("repair 0 = %+v, %v", m.Repair, err)
	}
	m, err = Parse([]byte(`{"repair": {"max_attempts": "unlimited"}}`))
	if err != nil || m.Repair == nil || !m.Repair.MaxAttempts.Valid || !m.Repair.MaxAttempts.Unlimited {
		t.Fatalf("repair unlimited = %+v, %v", m.Repair, err)
	}
	m, err = Parse([]byte(`{}`))
	if err != nil || m.Repair != nil {
		t.Fatalf("missing repair = %+v, %v", m.Repair, err)
	}
	m, err = Parse([]byte(`{"repair": {"max_attempts": null}}`))
	if err != nil || m.Repair == nil || m.Repair.MaxAttempts.Set {
		t.Fatalf("repair null = %+v, %v", m.Repair, err)
	}
	// Invalid values stay parseable but invalid: never unbounded by accident.
	for _, bad := range []string{
		`{"repair": {"max_attempts": "lots"}}`,
		`{"repair": {"max_attempts": -1}}`,
		`{"repair": {"max_attempts": 2.5}}`,
		`{"repair": {"max_attempts": true}}`,
		`{"repair": {"max_attempts": [2]}}`,
		`{"repair": {"max_attempts": "3"}}`,
	} {
		m, err := Parse([]byte(bad))
		if err != nil {
			t.Errorf("Parse(%s) failed the manifest: %v", bad, err)
			continue
		}
		if m.Repair == nil || !m.Repair.MaxAttempts.Set || m.Repair.MaxAttempts.Valid || m.Repair.MaxAttempts.Unlimited {
			t.Errorf("Parse(%s) = %+v, want set-but-invalid", bad, m.Repair)
		}
	}
	// A misspelled repair key still fails loudly.
	if _, err := Parse([]byte(`{"repair": {"max_attempt": 3}}`)); err == nil {
		t.Error("Parse(repair.max_attempt) accepted a misspelled key")
	}
	// Repair composes with the other keys.
	m, err = Parse([]byte(`{"server": {"args": ["serve", "$ADDR"]}, "repair": {"max_attempts": 1}}`))
	if err != nil || m.Server == nil || m.Repair == nil || m.Repair.MaxAttempts.N != 1 {
		t.Fatalf("repair with server = %+v, %v", m, err)
	}
}
