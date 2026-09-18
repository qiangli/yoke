package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestResourceAlertUnchangedTransactionSkipsPersistence(t *testing.T) {
	dir := isolateObservation(t)
	ctx := context.Background()
	first, err := UpdateAlertState(ctx, dir, func(l *AlertLedger) error { l.Entries["one"] = json.RawMessage(`{"value":1}`); return nil })
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dir, "alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		next, err := UpdateAlertState(ctx, dir, func(l *AlertLedger) error { l.Entries["one"] = json.RawMessage(`{"value":1}`); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if next.Revision != first.Revision || !next.UpdatedAt.Equal(first.UpdatedAt) {
			t.Fatal("identical client refresh changed ledger revision")
		}
	}
	after, _ := os.Stat(filepath.Join(dir, "alerts.json"))
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("identical transaction replaced durable ledger")
	}
}

func TestResourceAlertTransactionDetectsInPlaceMutation(t *testing.T) {
	dir := isolateObservation(t)
	ctx := context.Background()
	first, err := UpdateAlertState(ctx, dir, func(l *AlertLedger) error {
		l.Entries["one"] = json.RawMessage(`{"value":1}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := UpdateAlertState(ctx, dir, func(l *AlertLedger) error {
		l.Entries["one"][9] = '2'
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != first.Revision+1 {
		t.Fatal("in-place mutation did not advance revision")
	}
	persisted, err := ReadAlertState(ctx, dir)
	if err != nil || string(persisted.Entries["one"]) != `{"value":2}` {
		t.Fatalf("in-place mutation was lost: %v, %v", persisted, err)
	}
	next, err = UpdateAlertState(ctx, dir, func(l *AlertLedger) error {
		delete(l.Entries, "one")
		l.Entries["two"] = json.RawMessage(`{"value":2}`)
		return nil
	})
	if err != nil || next.Revision != persisted.Revision+1 {
		t.Fatalf("same-size key replacement was lost: %v, %v", next, err)
	}
}

func BenchmarkResourceAlertUnchangedTransaction(b *testing.B) {
	dir := isolateObservation(b)
	ctx := context.Background()
	_, err := UpdateAlertState(ctx, dir, func(l *AlertLedger) error {
		for i := 0; i < 40; i++ {
			l.Entries[fmt.Sprint(i)] = json.RawMessage(`{"condition":"memory","active":false,"observed_at":"2026-09-08T00:00:00Z","recovery_at":"2026-09-08T00:00:00Z","owner":"sprint-manager","revision":123,"delivery":{"key":"memory/sprint/138","done":true}}`)
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := UpdateAlertState(ctx, dir, func(*AlertLedger) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}
