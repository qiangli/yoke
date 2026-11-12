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
	// --name alone relabels without touching the circle.
	if got := run(alice, "set", "bob@example.com", "--name", "Bobby")["member"].(map[string]any); got["name"] != "Bobby" || got["circle"] != "team" {
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
	for _, circle := range []string{"Bad Circle", "owner", "public"} {
		cmd := NewMemberCmd()
		cmd.SetArgs([]string{"invite", "bob@example.com", "--circle", circle})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "invalid circle") && !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("circle %q: %v", circle, err)
		}
	}
}

func TestMemberCustomCircleRoundTrip(t *testing.T) {
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
	invitation := run(alice, "invite", "bob@example.com", "--circle", "hiking-club")["invitation"].(map[string]any)["id"].(string)
	run(bob, "accept", invitation, "--circle", "work-2026")
	if got := run(alice, "ls", "--circle", "hiking-club"); len(got["members"].([]any)) != 1 {
		t.Fatal(got)
	}
	run(alice, "set", "bob@example.com", "--circle", "x1")
	if got := run(alice, "check", "bob@example.com"); got["allowed"] != true || got["circle"] != "x1" {
		t.Fatal(got)
	}
	if got := run(alice, "ls", "--circle", "hiking-club"); len(got["members"].([]any)) != 0 {
		t.Fatal(got)
	}
}

func TestSuiteVerbsAgainstWire(t *testing.T) {
	f := cloudboxfake.New(t)
	alice := f.AddOwner("alice@example.com")
	other := f.AddOwner("other@example.com")
	// Pair through the public endpoints, then exercise CLI set/add/share/unshare/rm.
	c, err := NewSuiteClient(f.URL(), alice)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.Do(t.Context(), "POST", "/api/v1/hosts/invites", nil)
	if err != nil {
		t.Fatal(err)
	}
	var invite map[string]string
	if err = json.Unmarshal(raw, &invite); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Do(t.Context(), "POST", "/api/v1/hosts/pair", map[string]string{"code": invite["code"], "host": "studio"}); err != nil {
		t.Fatal(err)
	}
	run := func(tok string, args ...string) map[string]any {
		t.Helper()
		cmd := NewSuiteCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--master", f.URL(), "--token", tok}, args...))
		if e := cmd.Execute(); e != nil {
			t.Fatalf("suite %v: %v", args, e)
		}
		var v map[string]any
		if e := json.Unmarshal(out.Bytes(), &v); e != nil {
			t.Fatalf("suite output %q: %v", out.String(), e)
		}
		return v
	}
	run(alice, "set", "bundle", "studio/chat")
	run(alice, "add", "bundle", "studio/mail")
	if len(run(alice, "show", "bundle")["suite"].(map[string]any)["apps"].([]any)) != 2 {
		t.Fatal("suite set/add failed")
	}
	run(alice, "remove", "bundle", "studio/mail")
	if len(run(alice, "show", "bundle")["suite"].(map[string]any)["apps"].([]any)) != 1 {
		t.Fatal("suite remove failed")
	}
	grant := run(alice, "share", "bundle", "--circle", "team")["share"].(map[string]any)
	if len(run(alice, "shares", "bundle")["shares"].([]any)) != 1 {
		t.Fatal("suite shares failed")
	}
	run(alice, "unshare", "bundle", grant["id"].(string))
	run(alice, "rm", "bundle")
	cmd := NewSuiteCmd()
	cmd.SetArgs([]string{"--master", f.URL(), "--token", other, "show", "bundle"})
	if e := cmd.Execute(); e == nil {
		t.Fatal("other owner unexpectedly accessed suite")
	}
}
