package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	add := run(alice, "add", "quiet@example.com", "--circle", "family")
	if got := add["member"].(map[string]any); got["status"] != "pending" || got["circle"] != "family" || add["email_status"] != "skipped" {
		t.Fatal(got)
	}
	invite := run(alice, "invite", "bob@example.com", "--circle", "family")
	if invite["email_status"] != "sent" || invite["email_sent"] != true || invite["resent"] != false {
		t.Fatal(invite)
	}
	invitation := invite["invitation"].(map[string]any)["id"].(string)
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

func TestMemberInviteDelivery(t *testing.T) {
	f := cloudboxfake.New(t)
	const invitee = "bob@example.com"
	alice := f.AddOwner("alice@example.com")
	bob := f.AddOwner(invitee)
	run := func(token string, args ...string) string {
		t.Helper()
		cmd := NewMemberCmd()
		var out, stderr bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		cmd.SetArgs(append([]string{"--master", f.URL(), "--token", token}, args...))
		if err := cmd.Execute(); err != nil || stderr.Len() != 0 {
			t.Fatalf("member %v returned %v, stderr %q", args, err, stderr.String())
		}
		return out.String()
	}
	assertInvite := func(email, status, mailLine string) string {
		t.Helper()
		got := run(alice, "invite", email)
		id, _, _ := strings.Cut(got, "\t")
		want := fmt.Sprintf("%s\t%s\tguest\t%s\n%s\n", id, email, status, mailLine)
		if !strings.HasPrefix(id, "mem_") || got != want {
			t.Fatalf("invite %s: got %q, want member row and %q", email, got, mailLine)
		}
		return id
	}

	id := assertInvite(invitee, "pending", "invitation mail sent")
	if got := assertInvite(invitee, "pending", "re-sent invitation mail"); got != id {
		t.Fatalf("re-invite changed member ID from %q to %q", id, got)
	}
	var resent struct {
		Invitation cloudboxfake.Invitation `json:"invitation"`
		Resent     bool                    `json:"resent"`
	}
	if err := json.Unmarshal([]byte(run(alice, "invite", invitee, "--json")), &resent); err != nil || !resent.Resent || resent.Invitation.ID == "" {
		t.Fatalf("re-invite JSON: %+v, %v", resent, err)
	}
	run(bob, "accept", resent.Invitation.ID)
	assertInvite(invitee, "active", "already a member; no mail")

	// Compare the active-member JSON output directly with the fake's wire body.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/members", strings.NewReader(`{"email":"bob@example.com","circle":"guest"}`))
	request.Header.Set("Authorization", "Bearer "+alice)
	response := httptest.NewRecorder()
	f.Master.Config.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("X-Invitation-Email") != "skipped" {
		t.Fatalf("active member response: %d, %v", response.Code, response.Header())
	}
	if got := run(alice, "invite", invitee, "--json"); got != response.Body.String() {
		t.Fatalf("invite --json got %q, want %q", got, response.Body.String())
	}

	f.SetInvitationMailConfigured(false)
	assertInvite("carol@example.com", "pending", "invitation mail NOT sent: mail not configured; invitation not sent")
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
