package lenscmd

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

type fakeDelta struct {
	records, next, expiry string
	err                   error
	gotToken              string
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
