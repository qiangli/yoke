package provider_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

func TestFileJournalRoundTripAndTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	j, err := provider.NewFileJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	for i, ph := range []provider.Phase{provider.PhaseIntent, provider.PhaseResult} {
		e := provider.JournalEntry{At: at.Add(time.Duration(i) * time.Second), Provider: "p/1", Op: provider.OpCreate, IdempotencyKey: "k", Phase: ph, ResultID: "r-1", State: provider.StateRunning}
		if err := j.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Append(ctx, provider.JournalEntry{At: at, Provider: "other", Op: provider.OpStop, IdempotencyKey: "s", Phase: provider.PhaseIntent}); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves a partial last line; it is ignored.
	f, _ := os.OpenFile(filepath.Join(dir, "p_1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"at":"2026-10-20T01:02:05Z","provider":"p/1","op":"destroy","idempo`)
	f.Close()

	got, err := j.Load(ctx, "p/1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 || got[1].Phase != provider.PhaseResult || got[1].ResultID != "r-1" {
		t.Fatalf("loaded %+v", got)
	}
	v := provider.Replay(got)
	op, ok := v.Find(provider.OpCreate, "k")
	if !ok || !op.Done || op.Pending || op.ResourceID != "r-1" {
		t.Fatalf("replay %+v", op)
	}
	if r, ok := v.Resource("r-1"); !ok || r.State != provider.StateRunning || r.CreateKey != "k" {
		t.Fatalf("resource %+v", r)
	}
	if none, _ := j.Load(ctx, "missing"); none != nil {
		t.Fatalf("missing provider: %v", none)
	}
	info, _ := os.Stat(filepath.Join(dir, "p_1.jsonl"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal file mode %v", info.Mode())
	}
}

func TestDefaultJournalDirHonorsBashyHome(t *testing.T) {
	t.Setenv("BASHY_HOME", filepath.Join(t.TempDir(), "bh"))
	dir, err := provider.DefaultJournalDir()
	if err != nil || dir != filepath.Join(os.Getenv("BASHY_HOME"), "provider", "journal") {
		t.Fatalf("dir=%s err=%v", dir, err)
	}
	t.Setenv("BASHY_HOME", "")
	t.Setenv("HOME", t.TempDir())
	dir, err = provider.DefaultJournalDir()
	if err != nil || !strings.HasPrefix(dir, os.Getenv("HOME")) || !strings.HasSuffix(dir, filepath.Join(".bashy", "provider", "journal")) {
		t.Fatalf("dir=%s err=%v", dir, err)
	}
}

func TestReplayPendingAndManual(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	es := []provider.JournalEntry{
		{At: at, Provider: "p", Op: provider.OpCreate, IdempotencyKey: "a", Phase: provider.PhaseIntent, Attempt: 1},
		{At: at.Add(1), Provider: "p", Op: provider.OpCreate, IdempotencyKey: "b", Phase: provider.PhaseIntent, Attempt: 1},
		{At: at.Add(2), Provider: "p", Op: provider.OpCreate, IdempotencyKey: "b", Phase: provider.PhaseNeedsManualReconcile, Attempt: 1},
		{At: at.Add(3), Provider: "p", Op: provider.OpDestroy, IdempotencyKey: "d", ResourceID: "r-9", Phase: provider.PhaseRefused, Cost: provider.RefusedDestroyCost()},
	}
	v := provider.Replay(es)
	if p := v.Pending(); len(p) != 1 || p[0].IdempotencyKey != "a" {
		t.Fatalf("pending %+v", p)
	}
	if m := v.ManualReconcile(); len(m) != 1 || m[0].IdempotencyKey != "b" {
		t.Fatalf("manual %+v", m)
	}
	if r, ok := v.Resource("r-9"); !ok || r.Destroyed || r.Cost == nil || !r.Cost.Ongoing {
		t.Fatalf("refused destroy resource %+v", r)
	}
}
