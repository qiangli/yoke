package lenscmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/room"
)

type fakeDelta struct {
	records, next, expiry string
	err                   error
	gotToken              string
}

func TestAgentWorldBusSinceIncludesQuotaObservation(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	if err := room.Emit(room.Event{Type: room.EventUsage, Actor: "quotaobs", Topic: room.TopicQuotaObservation, Body: `{"schema":"bashy-quota-observation-v1"}`}); err != nil {
		t.Fatal(err)
	}
	src := busTimelineDelta{}
	loc := ref.Locator{Scheme: "bus", Target: "timeline"}
	records, cursor, _, err := src.Delta(context.Background(), loc, "")
	if err != nil {
		t.Fatal(err)
	}
	var event room.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(records))), &event); err != nil || event.Topic != room.TopicQuotaObservation {
		t.Fatalf("delta=%q event=%+v err=%v", records, event, err)
	}
	records, _, _, err = src.Delta(context.Background(), loc, cursor)
	if err != nil || len(records) != 0 {
		t.Fatalf("replay=%q err=%v", records, err)
	}
}

func (f *fakeDelta) Delta(_ context.Context, _ ref.Locator, token string) ([]byte, string, string, error) {
	f.gotToken = token
	return []byte(f.records), f.next, f.expiry, f.err
}

func withDelta(t *testing.T, scheme string, src DeltaSource) {
	t.Helper()
	old, had := deltaSources[scheme]
	if err := RegisterDeltaSource(scheme, src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			deltaSources[scheme] = old
		} else {
			delete(deltaSources, scheme)
		}
	})
}

func TestAgentWorldSinceUnregistered(t *testing.T) {
	delete(deltaSources, "kb")
	out, stderr, code := runTool(t, t.TempDir(), "", "--since=start", "-t", "jsonl", "kb:page")
	if code == 0 || out != "" || !strings.Contains(stderr, `no delta source registered for provider "kb"`) {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestAgentWorldSinceResume(t *testing.T) {
	src := &fakeDelta{records: "{\"n\":1}\n", next: "p1"}
	withDelta(t, "kb", src)
	dir := t.TempDir()
	out, stderr, code := runTool(t, dir, "", "--since=start", "-t", "jsonl", "kb:page")
	if code != 0 || stderr != "" {
		t.Fatalf("%d %s", code, stderr)
	}
	data, cursor := deltaCursor(t, out)
	if data != "{\"n\":1}" || src.gotToken != "" {
		t.Fatalf("%q %q", data, src.gotToken)
	}
	src.next = "p2"
	if _, stderr, code = runTool(t, dir, "", "--since", cursor, "-t", "jsonl", "kb:page"); code != 0 || src.gotToken != "p1" {
		t.Fatalf("%d %s %q", code, stderr, src.gotToken)
	}
	// Changed target or provider binding is refused with restart guidance.
	for _, loc := range []string{"kb:other", "host:page"} {
		withDelta(t, "host", src)
		out, stderr, code = runTool(t, dir, "", "--since", cursor, "-t", "jsonl", loc)
		if code == 0 || out != "" || !strings.Contains(stderr, "restart with --since=start") {
			t.Fatalf("%s: %d %q %q", loc, code, out, stderr)
		}
	}
}

func TestAgentWorldSinceExpiryAndFailureNoCursor(t *testing.T) {
	src := &fakeDelta{records: "{\"n\":1}\n", next: "p1", expiry: "2000-01-01T00:00:00Z"}
	withDelta(t, "kb", src)
	dir := t.TempDir()
	out, _, code := runTool(t, dir, "", "--since=start", "-t", "jsonl", "kb:page")
	_, cursor := deltaCursor(t, out)
	if code != 0 {
		t.Fatal(code)
	}
	_, stderr, code := runTool(t, dir, "", "--since", cursor, "-t", "jsonl", "kb:page")
	if code == 0 || !strings.Contains(stderr, "cursor expired; restart with --since=start") {
		t.Fatalf("%d %s", code, stderr)
	}
	src.err = ErrDeltaExpired
	out, stderr, code = runTool(t, dir, "", "--since=start", "-t", "jsonl", "kb:page")
	if code == 0 || out != "" || !strings.Contains(stderr, "restart with --since=start") {
		t.Fatalf("%d %q %s", code, out, stderr)
	}
	src.err, src.records = nil, "{bad\n"
	out, _, code = runTool(t, dir, "", "--since=start", "-t", "jsonl", "kb:page")
	if code == 0 || out != "" {
		t.Fatalf("invalid records emitted: %d %q", code, out)
	}
}

func TestAgentWorldSinceNonAgentWorldLocatorStillRejected(t *testing.T) {
	_, stderr, code := runTool(t, t.TempDir(), "", "--since=start", "-t", "jsonl", "dir:.")
	if code == 0 || !strings.Contains(stderr, "do not support this provider") {
		t.Fatalf("%d %s", code, stderr)
	}
}

func TestAgentWorldSinceUnsupportedModesSafeRejection(t *testing.T) {
	src := &fakeDelta{records: "{\"n\":1}\n", next: "p1"}
	withDelta(t, "kb", src)
	dir := t.TempDir()

	// fakeDelta does not implement TimeDeltaSource: --since=10m must be safely rejected
	out, stderr, code := runTool(t, dir, "", "--since=10m", "-t", "jsonl", "kb:page")
	if code == 0 || out != "" || !strings.Contains(stderr, `provider "kb" does not support time/duration in --since`) {
		t.Fatalf("expected unsupported time rejection, got code=%d out=%q stderr=%q", code, out, stderr)
	}

	// fakeDelta does not implement SeqDeltaSource: --since=42 must be safely rejected
	out, stderr, code = runTool(t, dir, "", "--since=42", "-t", "jsonl", "kb:page")
	if code == 0 || out != "" || !strings.Contains(stderr, `provider "kb" does not support sequence in --since`) {
		t.Fatalf("expected unsupported sequence rejection, got code=%d out=%q stderr=%q", code, out, stderr)
	}

	// start and valid cursor still work
	out, stderr, code = runTool(t, dir, "", "--since=start", "-t", "jsonl", "kb:page")
	if code != 0 || stderr != "" {
		t.Fatalf("expected start to succeed, got code=%d stderr=%q", code, stderr)
	}
}

type translatingDelta struct {
	fakeDelta
	translatedSeq  string
	translatedTime string
}

func (t *translatingDelta) TranslateSeq(_ context.Context, _ ref.Locator, seq int64) (string, error) {
	return t.translatedSeq, nil
}

func (t *translatingDelta) TranslateTime(_ context.Context, _ ref.Locator, _ time.Time) (string, error) {
	return t.translatedTime, nil
}

func TestAgentWorldSinceTranslatingProvider(t *testing.T) {
	src := &translatingDelta{
		fakeDelta:      fakeDelta{records: "{\"n\":2}\n", next: "p2"},
		translatedSeq:  "p1-seq",
		translatedTime: "p1-time",
	}
	withDelta(t, "kb", src)
	dir := t.TempDir()

	// --since=42 calls TranslateSeq and passes translated token
	out, stderr, code := runTool(t, dir, "", "--since=42", "-t", "jsonl", "kb:page")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	data, _ := deltaCursor(t, out)
	if data != "{\"n\":2}" || src.gotToken != "p1-seq" {
		t.Fatalf("data=%q gotToken=%q, want p1-seq", data, src.gotToken)
	}

	// --since=10m calls TranslateTime and passes translated token
	out, stderr, code = runTool(t, dir, "", "--since=10m", "-t", "jsonl", "kb:page")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	data, _ = deltaCursor(t, out)
	if data != "{\"n\":2}" || src.gotToken != "p1-time" {
		t.Fatalf("data=%q gotToken=%q, want p1-time", data, src.gotToken)
	}
}
