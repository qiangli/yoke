package quotaobs

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecordReadAndDedupe(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	input := Observation{At: now, Source: "weave", Account: "seat-a", Excerpt: "usage limit Bearer abcdefghijklmnopqrstuvwxyz try again at 1 PM", Headers: map[string]string{"Retry-After": "120", "X-RateLimit-Remaining": strings.Repeat("x", 200), "Authorization": "secret"}}
	if err := Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.At = now.Add(9 * time.Minute)
	if err := Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	got, err := Read(time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("dedupe: got %d", len(got))
	}
	if strings.Contains(got[0].Excerpt, "abcdefghijklmnopqrstuvwxyz") || !strings.Contains(got[0].Excerpt, "[REDACTED]") {
		t.Fatalf("excerpt: %q", got[0].Excerpt)
	}
	if got[0].Schema != Schema || len(got[0].ExcerptSHA) != 64 {
		t.Fatalf("metadata: %+v", got[0])
	}
	if len(got[0].Excerpt) > 2048 {
		t.Fatalf("excerpt too long: %d", len(got[0].Excerpt))
	}
	if len(got[0].Headers) != 2 || got[0].Headers["retry-after"] != "120" || len(got[0].Headers["x-ratelimit-remaining"]) != 128 {
		t.Fatalf("headers: %#v", got[0].Headers)
	}
	input.At = now.Add(11 * time.Minute)
	if err := Record(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	got, err = Read(now.Add(10*time.Minute), func(o Observation) bool { return o.Account == "seat-a" })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("window/read filter: %d", len(got))
	}
	p, _ := path()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v", st.Mode())
	}
	if dir, err := os.Stat(strings.TrimSuffix(p, "/observations.jsonl")); err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v, %v", dir, err)
	}
}

func TestRotationAndTornLine(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	p, _ := path()
	if err := os.MkdirAll(strings.TrimSuffix(p, "/observations.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	old := Observation{Schema: Schema, At: time.Now(), Source: "manual", Excerpt: strings.Repeat("x", maxFile), ExcerptSHA: "old"}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(p, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Record(context.Background(), Observation{Source: "weave", Excerpt: "quota reached"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema":"incomplete"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := Read(time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rotation/torn line: %d", len(got))
	}
}
