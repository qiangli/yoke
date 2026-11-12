package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

func TestGroupVerbsAgainstWire(t *testing.T) {
	f := cloudboxfake.New(t)
	alice := f.AddOwner("alice@example.com")
	bob := f.AddOwner("bob@example.com")

	// Set up mutual membership: alice invites bob, bob accepts
	memCmd := NewMemberCmd()
	memCmd.SetArgs([]string{"--master", f.URL(), "--token", alice, "--json", "invite", "bob@example.com", "--circle", "team"})
	var invOut bytes.Buffer
	memCmd.SetOut(&invOut)
	if err := memCmd.Execute(); err != nil {
		t.Fatalf("member invite failed: %v", err)
	}
	var invEnv map[string]any
	if err := json.Unmarshal(invOut.Bytes(), &invEnv); err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	invID := invEnv["invitation"].(map[string]any)["id"].(string)

	acceptCmd := NewMemberCmd()
	acceptCmd.SetArgs([]string{"--master", f.URL(), "--token", bob, "--json", "accept", invID, "--circle", "friend"})
	if err := acceptCmd.Execute(); err != nil {
		t.Fatalf("member accept failed: %v", err)
	}

	runGroup := func(token string, args ...string) (map[string]any, string, error) {
		t.Helper()
		cmd := NewGroupCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--master", f.URL(), "--token", token}, args...))
		err := cmd.Execute()
		var m map[string]any
		_ = json.Unmarshal(out.Bytes(), &m)
		return m, out.String(), err
	}

	// 1. Dry run group add
	dryAdd, _, err := runGroup(alice, "add", "devs", "bob@example.com", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run add: %v", err)
	}
	if dryAdd["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %v", dryAdd)
	}

	// Verify nothing was created by dry-run
	lsEmpty, _, err := runGroup(alice, "ls", "--json")
	if err != nil {
		t.Fatalf("group ls: %v", err)
	}
	if len(lsEmpty["groups"].([]any)) != 0 {
		t.Fatalf("dry run created a group: %v", lsEmpty)
	}

	// 2. Non-member refused: 400 not_a_member surfaces as a clear error
	_, _, err = runGroup(alice, "add", "devs", "stranger@example.com")
	if err == nil {
		t.Fatal("expected error adding non-member, got nil")
	}
	if !strings.Contains(err.Error(), "not_a_member") {
		t.Fatalf("expected not_a_member in error, got: %v", err)
	}

	// 3. Owner allowed in a group
	addOwner, _, err := runGroup(alice, "add", "devs", "alice@example.com", "--json")
	if err != nil {
		t.Fatalf("add owner to group: %v", err)
	}
	groups := addOwner["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["name"] != "devs" {
		t.Fatalf("unexpected add group response: %v", addOwner)
	}

	// 4. Add member bob to group
	addBob, _, err := runGroup(alice, "add", "devs", "bob@example.com", "--json")
	if err != nil {
		t.Fatalf("add bob to group: %v", err)
	}
	members := addBob["groups"].([]any)[0].(map[string]any)["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("expected 2 members in devs, got %d", len(members))
	}

	// 5. Group ls round-trip
	lsAll, _, err := runGroup(alice, "ls", "--json")
	if err != nil {
		t.Fatalf("group ls: %v", err)
	}
	if len(lsAll["groups"].([]any)) != 1 {
		t.Fatalf("expected 1 group, got %d", len(lsAll["groups"].([]any)))
	}

	// Group ls by name
	lsDevs, _, err := runGroup(alice, "ls", "devs", "--json")
	if err != nil {
		t.Fatalf("group ls devs: %v", err)
	}
	if len(lsDevs["groups"].([]any)) != 1 {
		t.Fatalf("expected 1 group matching devs, got %d", len(lsDevs["groups"].([]any)))
	}

	// Group ls plain text
	_, plainLs, err := runGroup(alice, "ls", "devs")
	if err != nil {
		t.Fatalf("group ls plain: %v", err)
	}
	if !strings.Contains(plainLs, "alice@example.com") || !strings.Contains(plainLs, "bob@example.com") {
		t.Fatalf("plain ls output missing members: %q", plainLs)
	}

	// 6. Group rm with dry-run
	dryRm, _, err := runGroup(alice, "rm", "devs", "bob@example.com", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run rm: %v", err)
	}
	if dryRm["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %v", dryRm)
	}

	// Group rm actual
	rmRes, _, err := runGroup(alice, "rm", "devs", "bob@example.com", "--json")
	if err != nil {
		t.Fatalf("group rm: %v", err)
	}
	if rmRes["removed"] != true {
		t.Fatalf("expected removed=true, got %v", rmRes)
	}

	// Verify bob was removed
	lsAfterRm, _, err := runGroup(alice, "ls", "devs", "--json")
	if err != nil {
		t.Fatalf("group ls: %v", err)
	}
	membersAfterRm := lsAfterRm["groups"].([]any)[0].(map[string]any)["members"].([]any)
	if len(membersAfterRm) != 1 || membersAfterRm[0].(string) != "alice@example.com" {
		t.Fatalf("expected only alice after rm, got %v", membersAfterRm)
	}

	// 7. Group delete with dry-run
	dryDel, _, err := runGroup(alice, "delete", "devs", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run delete: %v", err)
	}
	if dryDel["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %v", dryDel)
	}

	// Group delete actual
	delRes, _, err := runGroup(alice, "delete", "devs", "--json")
	if err != nil {
		t.Fatalf("group delete: %v", err)
	}
	if delRes["removed"] != true {
		t.Fatalf("expected removed=true, got %v", delRes)
	}

	// Verify group is deleted
	lsAfterDel, _, err := runGroup(alice, "ls", "--json")
	if err != nil {
		t.Fatalf("group ls: %v", err)
	}
	if len(lsAfterDel["groups"].([]any)) != 0 {
		t.Fatalf("expected 0 groups after delete, got %d", len(lsAfterDel["groups"].([]any)))
	}
}

func TestShareWhoVerbsAgainstWire(t *testing.T) {
	f := cloudboxfake.New(t)
	alice := f.AddOwner("alice@example.com")
	bob := f.AddOwner("bob@example.com")
	carol := f.AddOwner("carol@example.com")
	dave := f.AddOwner("dave@example.com")

	// Pair host "studio" for alice
	c, err := NewMemberClient(f.URL(), alice)
	if err != nil {
		t.Fatal(err)
	}
	invRaw, err := c.Do(t.Context(), "POST", "/api/v1/hosts/invites", nil)
	if err != nil {
		t.Fatal(err)
	}
	var inv map[string]string
	_ = json.Unmarshal(invRaw, &inv)
	if _, err := c.Do(t.Context(), "POST", "/api/v1/hosts/pair", map[string]string{"code": inv["code"], "host": "studio"}); err != nil {
		t.Fatal(err)
	}

	makeMutual := func(ownerTok, memberTok, email, circle string) {
		invR, err := c.Do(t.Context(), "POST", "/api/v1/members", map[string]string{"email": email, "circle": circle})
		if err != nil {
			t.Fatal(err)
		}
		var env map[string]any
		_ = json.Unmarshal(invR, &env)
		invID := env["invitation"].(map[string]any)["id"].(string)

		cMember, err := NewMemberClient(f.URL(), memberTok)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cMember.Do(t.Context(), "POST", "/api/v1/member/invitations/"+invID+"/accept", nil); err != nil {
			t.Fatal(err)
		}
	}

	makeMutual(alice, bob, "bob@example.com", "team")
	makeMutual(alice, carol, "carol@example.com", "team")
	makeMutual(alice, dave, "dave@example.com", "guest")

	// Create group "alpha" with bob and dave
	if _, err := c.Do(t.Context(), "POST", "/api/v1/groups/alpha/members", map[string]any{"emails": []string{"bob@example.com", "dave@example.com"}}); err != nil {
		t.Fatal(err)
	}

	runWho := func(token, arg string, asJSON bool) (string, error) {
		t.Helper()
		cmd := NewShareCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		args := []string{"--master", f.URL(), "--token", token, "who", arg}
		if asJSON {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		err := cmd.Execute()
		return strings.TrimSpace(out.String()), err
	}

	// 1. Empty list of rules -> owner only (who returns empty emails list)
	out, err := runWho(alice, "studio/unshared", true)
	if err != nil {
		t.Fatalf("who empty: %v", err)
	}
	var emptyEnv struct {
		Host   string   `json:"host"`
		App    string   `json:"app"`
		Owner  string   `json:"owner"`
		Emails []string `json:"emails"`
	}
	if err := json.Unmarshal([]byte(out), &emptyEnv); err != nil {
		t.Fatalf("decode who empty: %v", err)
	}
	if emptyEnv.Owner != "alice@example.com" || len(emptyEnv.Emails) != 0 {
		t.Fatalf("unexpected who empty result: %v", emptyEnv)
	}

	// 2. Circle rule: share app "circle-app" with circle "team"
	if _, err := c.Do(t.Context(), "POST", "/api/v1/hosts/studio/shares", map[string]any{
		"app":     "circle-app",
		"circles": []string{"team"},
		"groups":  []string{},
		"emails":  []string{},
	}); err != nil {
		t.Fatal(err)
	}

	// Plain text output: one email per line
	plainOut, err := runWho(alice, "studio/circle-app", false)
	if err != nil {
		t.Fatalf("who plain: %v", err)
	}
	lines := strings.Split(plainOut, "\n")
	if len(lines) != 2 || lines[0] != "bob@example.com" || lines[1] != "carol@example.com" {
		t.Fatalf("unexpected plain who output: %q", plainOut)
	}

	// JSON output
	jsonOut, err := runWho(alice, "studio/circle-app", true)
	if err != nil {
		t.Fatalf("who json: %v", err)
	}
	var circleEnv struct {
		Emails []string `json:"emails"`
	}
	_ = json.Unmarshal([]byte(jsonOut), &circleEnv)
	if len(circleEnv.Emails) != 2 || circleEnv.Emails[0] != "bob@example.com" || circleEnv.Emails[1] != "carol@example.com" {
		t.Fatalf("unexpected json who: %v", circleEnv)
	}

	// 3. Circle + group rule: circles: ["team"], groups: ["alpha"]
	// Bob is team + alpha -> admitted. Carol is team + not alpha -> excluded. Dave is alpha + guest -> excluded.
	if _, err := c.Do(t.Context(), "POST", "/api/v1/hosts/studio/shares", map[string]any{
		"app":     "cg-app",
		"circles": []string{"team"},
		"groups":  []string{"alpha"},
		"emails":  []string{},
	}); err != nil {
		t.Fatal(err)
	}
	cgOut, err := runWho(alice, "studio/cg-app", true)
	if err != nil {
		t.Fatalf("who cg: %v", err)
	}
	var cgEnv struct {
		Emails []string `json:"emails"`
	}
	_ = json.Unmarshal([]byte(cgOut), &cgEnv)
	if len(cgEnv.Emails) != 1 || cgEnv.Emails[0] != "bob@example.com" {
		t.Fatalf("unexpected cg who: %v", cgEnv)
	}

	// 4. Email rule: emails: ["dave@example.com"]
	if _, err := c.Do(t.Context(), "POST", "/api/v1/hosts/studio/shares", map[string]any{
		"app":     "email-app",
		"circles": []string{},
		"groups":  []string{},
		"emails":  []string{"dave@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	emailOut, err := runWho(alice, "studio/email-app", true)
	if err != nil {
		t.Fatalf("who email: %v", err)
	}
	var emailEnv struct {
		Emails []string `json:"emails"`
	}
	_ = json.Unmarshal([]byte(emailOut), &emailEnv)
	if len(emailEnv.Emails) != 1 || emailEnv.Emails[0] != "dave@example.com" {
		t.Fatalf("unexpected email who: %v", emailEnv)
	}

	// 5. Malformed HOST/APP
	if _, err := runWho(alice, "studio", true); err == nil {
		t.Fatal("expected error on malformed HOST/APP without slash")
	}
	if _, err := runWho(alice, "studio/app/extra", true); err == nil {
		t.Fatal("expected error on malformed HOST/APP with extra slash")
	}

	// 6. Caller is not host owner -> fails
	if _, err := runWho(bob, "studio/circle-app", true); err == nil {
		t.Fatal("expected error when caller is not host owner")
	}
}
