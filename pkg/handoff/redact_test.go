package handoff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// handoffVaultFixture installs a vault-projected name with a rendered value
// in this process's environment (see bus.redact_body_test for the rationale:
// the value is not credential-shaped, so only exact registration masks it).
func handoffVaultFixture(t *testing.T, localName, value string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(xdg, "bashy", "secrets.map")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(localName+"=@vault-item\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(localName, value)
}

// A handoff record is a FILE meant to travel off-host (scp, mesh, an issue
// comment): the persisted artifact must never carry a vault value, in the
// brief, the diff, or an untracked file carried by content.
func TestSaveRedactsVaultValues(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	handoffVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")

	dir := t.TempDir()
	rec := &Record{
		ID:         "20261109T000000Z-abcd1234",
		CreatedAt:  time.Now().UTC(),
		Continuity: "mid-rotation: DEPLOY_TOKEN is sycamore-lantern-42 until Friday",
		NextAction: "run deploy with sycamore-lantern-42",
		Blockers:   []string{"waiting on sycamore-lantern-42 approval"},
		Work: WorkingState{
			Repo: dir,
			Diff: "+export DEPLOY_TOKEN=sycamore-lantern-42",
			Untracked: []UntrackedFile{
				{Path: "env.local", Content: "DEPLOY_TOKEN=sycamore-lantern-42\n"},
			},
		},
	}
	path, err := Save(dir, rec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sycamore-lantern-42") {
		t.Fatalf("vault value persisted to the handoff artifact:\n%s", b)
	}
	for _, want := range []string{
		"[redacted:DEPLOY_TOKEN]",
		"env.local", // the untracked file entry survives, only its content is masked
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("artifact missing %q:\n%s", want, b)
		}
	}
	// The in-memory record is redacted too, so the caller's later use of it
	// (the --json echo, a brief built from Continuity) cannot re-leak.
	if strings.Contains(rec.Continuity, "sycamore-lantern-42") {
		t.Fatalf("record kept plaintext after Save: %q", rec.Continuity)
	}
}

// The host event log is streamed to session members (bashy events, meet SSE):
// a handoff brief must be masked before it rides the timeline.
func TestRecordEventRedactsBrief(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	handoffVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")

	recordEvent(&Record{ID: "20261109T000000Z-abcd1234"}, "created", "tester",
		"handed off with token sycamore-lantern-42 in the brief")
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no handoff event recorded")
	}
	last := events[len(events)-1]
	if strings.Contains(last.Body, "sycamore-lantern-42") {
		t.Fatalf("vault value on the host event log: %q", last.Body)
	}
	if !strings.Contains(last.Body, "[redacted:DEPLOY_TOKEN]") {
		t.Fatalf("event brief missing marker: %q", last.Body)
	}
}
