package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJSONLRememberRecallRoundTripAndScoring(t *testing.T) {
	ctx := context.Background()
	st := NewJSONLStore(t.TempDir())
	older := time.Now().Add(-time.Hour).UTC()
	if err := st.Remember(ctx, Observation{IssueID: 1, Title: "unrelated", Outcome: "failed", FilesTouched: []string{"docs/a.md"}, CreatedAt: older}); err != nil {
		t.Fatal(err)
	}
	if err := st.Remember(ctx, Observation{IssueID: 2, Title: "fix parser", Outcome: "submitted", FilesTouched: []string{"pkg/parser.go", "pkg/token.go"}, CreatedAt: older.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Recall(ctx, Query{Files: []string{"pkg/parser.go"}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IssueID != 2 {
		t.Fatalf("recall ranked/matched wrong observations: %+v", got)
	}
}

func TestJSONLRecallSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")
	data := []byte("{bad json\n{\"issue_id\":7,\"outcome\":\"submitted\",\"summary\":\"ok\",\"created_at\":\"2026-01-01T00:00:00Z\"}\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NewJSONLStore(dir).Recall(context.Background(), Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IssueID != 7 {
		t.Fatalf("malformed line should be skipped, got %+v", got)
	}
}
