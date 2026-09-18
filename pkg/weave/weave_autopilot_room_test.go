package weave

import (
	"path/filepath"
	"strings"
	"testing"
)

// The campaign room follows the lease: acquiring opens one, releasing closes
// it, and the address is remembered in between so the close can find it.
func TestAutopilotRoom_FollowsTheLease(t *testing.T) {
	dir := t.TempDir()

	if c, err := loadAutopilotRoom(dir); err != nil || c != nil {
		t.Fatalf("a repo with no campaign has no room: %+v %v", c, err)
	}

	// The topic is keyed on the queue TAG, never the repo path. The bus topic
	// is visible to every subscriber, and a path would hand each of them the
	// origin location — the same containment the tag itself exists for.
	topic := autopilotAssignment(dir).Topic()
	if !strings.HasPrefix(topic, "conductor.") {
		t.Errorf("topic = %q, want a conductor topic", topic)
	}
	if strings.Contains(topic, string(filepath.Separator)) {
		t.Errorf("topic %q leaks a path — it must carry only the queue tag", topic)
	}
}

// A campaign room that cannot be opened must not stop the campaign. An
// autopilot with no intercom still drives the queue, and trading the work for
// the ability to discuss it would be the wrong way round.
func TestAutopilotRoom_FailureIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	line := autopilotRoomLine(dir, "tester")
	if line == "" {
		t.Fatal("the acquire line must always say something about reachability")
	}
	// Either it opened, or it says why not AND still gives the bus topic —
	// never silence, which would read as "no room was wanted".
	if !strings.Contains(line, "reachable at") && !strings.Contains(line, "no room") {
		t.Errorf("line = %q, want either an address or a stated reason", line)
	}
	if strings.Contains(line, "no room") && !strings.Contains(line, "bus conductor.") {
		t.Errorf("a failed open must still name the bus fallback: %q", line)
	}
}

// Closing a room nobody opened is not an error: the point of closing is that
// the room ends up shut, and a successor may have swept it first.
func TestAutopilotRoom_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := closeAutopilotRoom(dir, "tester"); err != nil {
		t.Errorf("closing an absent room should be a no-op, got %v", err)
	}
}
