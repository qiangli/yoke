package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func ambiguousSprintFleet(t *testing.T) *fleet.Catalog {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `agents:
  - name: duplicate-owner
    tool: codex
    model: one
  - name: DUPLICATE-OWNER
    tool: claude
    model: two
  - name: alpha-owner
    aliases: [shared-owner]
    tool: codex
    model: one
  - name: beta-owner
    aliases: [shared-owner]
    tool: claude
    model: two
`
	if err := os.WriteFile(filepath.Join(dir, "ambiguous.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
}

func TestSprintManagerRejectsAmbiguousAgentIdentity(t *testing.T) {
	cat := ambiguousSprintFleet(t)
	previous := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = previous })

	for _, owner := range []string{"duplicate-owner", "shared-owner"} {
		t.Run(owner, func(t *testing.T) {
			// Ambiguous, not unknown: the name is real and answers for more than
			// one principal, so the fix is to qualify it rather than register it.
			if err := validateSprintOwner(owner); err == nil ||
				!strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("ambiguous sprint manager %q was not rejected: %v", owner, err)
			}
		})
	}
}

// A reminder is silent when there is nothing waiting. A surface that speaks up
// with no news is one an agent learns to ignore, which would cost exactly the
// message it exists to protect.
func TestUnreadReminderIsSilentWithNoMail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := sprintUnreadReminder("nobody-home"); got != "" {
		t.Fatalf("reminder spoke with no unread mail: %q", got)
	}
}

// When it does speak it must carry the runnable command, not just a count.
// "You have mail" leaves the agent to work out how to read it; the point of a
// reminder at a busy moment is that acting on it costs nothing.
func TestUnreadReminderCarriesTheCommand(t *testing.T) {
	got := formatUnreadReminder(3, 12*time.Minute, "alice")
	for _, want := range []string{"3 unread", "12m", "bashy inbox --as alice"} {
		if !strings.Contains(got, want) {
			t.Errorf("reminder omits %q: %q", want, got)
		}
	}
}
