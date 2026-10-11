package commscli

// The messenger front door, driven the way a gateway hook drives it: argv into
// the built binary, the reply on stdout. Pairing goes through the app-pair
// store (`app pair approve`), approvals through the shared queue, and every
// message lands on the host event log.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ask"
)

var pairCodeRE = regexp.MustCompile(`pair code ([A-Z2-7]{4}-[A-Z2-7]{4})`)

func meetDMWorld(t *testing.T) (*world, map[string]string) {
	t.Helper()
	w := newWorld(t)
	base := filepath.Dir(w.home)
	vars := map[string]string{
		"BASHY_HOME":     filepath.Join(base, "bashy"),
		"BASHY_MEET_DIR": filepath.Join(base, "meet"),
		ask.DirEnv:       filepath.Join(base, "ask"),
	}
	return w, vars
}

func TestMeetDMPairingScopeApprovalRevokeAndEvents(t *testing.T) {
	w, vars := meetDMWorld(t)
	dm := func(sender string, text ...string) result {
		t.Helper()
		args := append([]string{"meet", "dm", "", "--channel", "slack", "--sender", sender}, text...)
		return w.run(t, vars, args...)
	}

	// Unpaired: a code, exit 0 (the reply is for the sender), nothing invoked.
	res := dm("ops", "hello")
	m := pairCodeRE.FindStringSubmatch(res.out)
	if res.code != 0 || m == nil {
		t.Fatalf("unpaired sender: exit %d out %q err %q", res.code, res.out, res.err)
	}
	code := m[1]

	// The operator approves the code for the approve scope only.
	if res := w.run(t, vars, "app", "pair", "approve", code, "--allow", "approve"); res.code != 0 ||
		!strings.Contains(res.out, "paired ops on channel slack") {
		t.Fatalf("approve: exit %d out %q err %q", res.code, res.out, res.err)
	}
	if res := w.run(t, vars, "app", "pair", "approve", code); res.code == 0 {
		t.Fatalf("a pair code must work exactly once: %q", res.out)
	}
	if res := w.run(t, vars, "app", "devices", "--json"); !strings.Contains(res.out, `"sender": "ops"`) {
		t.Fatalf("the sender is not listed as a device: %q", res.out)
	}

	// approve scope does not reach an agent.
	if res := dm("ops", "hello"); !strings.Contains(res.out, `needs scope "ask"`) {
		t.Fatalf("approve-only sender: %q %q", res.out, res.err)
	}

	// /approve settles an approval in the shared queue.
	t.Setenv(ask.DirEnv, vars[ask.DirEnv])
	t.Setenv("BASHY_ROOM_DIR", w.room)
	req, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Argv: []string{"git", "push"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := dm("ops", "/approve", req.ID); res.code != 0 || !strings.Contains(res.out, "allow "+req.ID) {
		t.Fatalf("/approve: exit %d out %q err %q", res.code, res.out, res.err)
	}
	if d, ok := ask.LoadDecision(req.ID); !ok || !d.Allowed() || d.By != "slack:ops" {
		t.Fatalf("decision = %+v ok=%v", d, ok)
	}
	if res := w.run(t, vars, "approve", "list"); strings.Contains(res.out, req.ID[:8]) {
		t.Fatalf("approval still pending after /approve: %q", res.out)
	}

	// Revoke puts the sender back behind pairing.
	if res := w.run(t, vars, "meet", "dm", "--channel", "slack", "--sender", "ops", "--revoke"); !strings.Contains(res.out, "revoked ops") {
		t.Fatalf("revoke: %q %q", res.out, res.err)
	}
	if res := dm("ops", "/approve", req.ID); pairCodeRE.FindString(res.out) == "" {
		t.Fatalf("revoked sender was not sent back to pairing: %q", res.out)
	}

	// Every message — paired, unpaired, command — is one inbound event.
	res = w.run(t, vars, "events", "--json", "--kind", "inbound")
	if n := strings.Count(res.out, `"actor":"slack:ops"`); n != 4 {
		t.Fatalf("inbound events for slack:ops = %d, want 4:\n%s", n, res.out)
	}
}
