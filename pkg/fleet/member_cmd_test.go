package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

func TestMemberVerbsAgainstWire(t *testing.T) {
	f := cloudboxfake.New(t)
	alice := f.AddOwner("alice@example.com")
	bob := f.AddOwner("bob@example.com")
	run := func(token string, args ...string) map[string]any {
		t.Helper()
		cmd := NewMemberCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--master", f.URL(), "--token", token, "--json"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("member %v: %v", args, err)
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("member %v output %q: %v", args, out.String(), err)
		}
		return result
	}
	if got := run(alice, "invite", "bob@example.com", "--circle", "family", "--dry-run"); got["dry_run"] != true {
		t.Fatal(got)
	}
	if got := run(alice, "ls"); len(got["members"].([]any)) != 0 {
		t.Fatal("dry run sent request")
	}
	invitation := run(alice, "invite", "bob@example.com", "--circle", "family")["invitation"].(map[string]any)["id"].(string)
	if got := run(alice, "check", "bob@example.com"); got["reason"] != "inactive" {
		t.Fatal(got)
	}
	run(bob, "accept", invitation, "--circle", "friend")
	if got := run(alice, "check", "bob@example.com"); got["allowed"] != true || got["circle"] != "family" {
		t.Fatal(got)
	}
	if got := run(bob, "of"); len(got["of"].([]any)) != 1 {
		t.Fatal(got)
	}
	if got := run(alice, "ls", "--circle", "team"); len(got["members"].([]any)) != 0 {
		t.Fatal(got)
	}
	run(alice, "set", "bob@example.com", "--circle", "team")
	if got := run(alice, "check", "bob@example.com"); got["circle"] != "team" {
		t.Fatal(got)
	}
	run(alice, "rm", "bob@example.com")
	if got := run(bob, "of"); len(got["of"].([]any)) != 0 {
		t.Fatal(got)
	}
	if got := run(bob, "check", "alice@example.com"); got["reason"] != "not_mutual" {
		t.Fatal(got)
	}
	if got := run(bob, "invitations"); len(got["invitations"].([]any)) != 0 {
		t.Fatal(got)
	}
}

func TestMemberRejectsBadCircleBeforeNetwork(t *testing.T) {
	cmd := NewMemberCmd()
	cmd.SetArgs([]string{"invite", "bob@example.com", "--circle", "stranger"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid circle") {
		t.Fatal(err)
	}
}
