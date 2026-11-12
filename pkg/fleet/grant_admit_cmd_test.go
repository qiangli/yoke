package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
	"github.com/spf13/cobra"
)

func runGrantCommand(t *testing.T, cmd *cobra.Command, args ...string) (map[string]any, string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var v map[string]any
	_ = json.Unmarshal(out.Bytes(), &v)
	return v, out.String(), err
}

func TestGrantFlagsAndListAgainstFake(t *testing.T) {
	f := cloudboxfake.New(t)
	owner := f.AddOwner("owner@example.com")
	c, e := NewMemberClient(f.URL(), owner)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := c.Do(t.Context(), "POST", "/api/v1/hosts/invites", nil)
	if e != nil {
		t.Fatal(e)
	}
	var invite map[string]string
	_ = json.Unmarshal(raw, &invite)
	if _, e = c.Do(t.Context(), "POST", "/api/v1/hosts/pair", map[string]string{"code": invite["code"], "host": "studio"}); e != nil {
		t.Fatal(e)
	}
	base := []string{"--master", f.URL(), "--token", owner}
	args := append(append([]string{}, base...), "add", "studio/chat", "--circle", "team", "--circle", "friend", "--group", "devs", "--email", "a@x", "--email", "b@x", "--json")
	got, _, e := runGrantCommand(t, NewShareCmd(), args...)
	if e != nil {
		t.Fatal(e)
	}
	share := got["share"].(map[string]any)
	for key, want := range map[string]int{"circles": 2, "groups": 1, "emails": 2} {
		if len(share[key].([]any)) != want {
			t.Fatalf("%s: %v", key, share)
		}
	}
	got, _, e = runGrantCommand(t, NewShareCmd(), append(append([]string{}, base...), "ls", "studio", "--json")...)
	if e != nil {
		t.Fatal(e)
	}
	if len(got["shares"].([]any)) != 1 {
		t.Fatalf("host list: %v", got)
	}
	_, plain, e := runGrantCommand(t, NewShareCmd(), append(append([]string{}, base...), "ls", "studio")...)
	if e != nil || !strings.Contains(plain, `"groups":["devs"]`) {
		t.Fatalf("host plain: %q %v", plain, e)
	}
	_, _, e = runGrantCommand(t, NewSuiteCmd(), append(append([]string{}, base...), "set", "work", "studio/chat")...)
	if e != nil {
		t.Fatal(e)
	}
	got, _, e = runGrantCommand(t, NewSuiteCmd(), append(append([]string{}, base...), "share", "work", "--circle", "team", "--group", "devs", "--email", "a@x")...)
	if e != nil {
		t.Fatal(e)
	}
	if len(got["share"].(map[string]any)["groups"].([]any)) != 1 {
		t.Fatalf("suite share: %v", got)
	}
	got, _, e = runGrantCommand(t, NewSuiteCmd(), append(append([]string{}, base...), "shares", "work")...)
	if e != nil || len(got["shares"].([]any)) != 1 {
		t.Fatalf("suite list: %v %v", got, e)
	}
}

func TestSatelliteAdmitLifecycleAgainstFake(t *testing.T) {
	f := cloudboxfake.New(t)
	owner := f.AddOwner("owner@example.com")
	other := f.AddOwner("other@example.com")
	sat := f.AddSatellite(t, "owner@example.com", "home")
	base := []string{"--master", f.URL(), "--token", owner}
	run := func(args ...string) (map[string]any, string, error) {
		return runGrantCommand(t, NewSatelliteCmd(), append(append([]string{}, base...), append([]string{"admit"}, args...)...)...)
	}
	got, _, e := run("ls", sat, "--json")
	if e != nil || len(got["rules"].([]any)) != 0 {
		t.Fatalf("empty: %v %v", got, e)
	}
	got, _, e = run("add", sat, "--circle", "team", "--group", "devs", "--email", "a@x", "--dry-run", "--json")
	if e != nil || got["dry_run"] != true {
		t.Fatalf("dry run: %v %v", got, e)
	}
	got, _, e = run("ls", sat, "--json")
	if e != nil || len(got["rules"].([]any)) != 0 {
		t.Fatalf("dry run wrote: %v %v", got, e)
	}
	got, _, e = run("add", sat, "--circle", "team", "--group", "devs", "--email", "a@x", "--json")
	if e != nil || len(got["rules"].([]any)) != 1 {
		t.Fatalf("add: %v %v", got, e)
	}
	rule := got["rules"].([]any)[0].(map[string]any)
	if len(rule["circles"].([]any)) != 1 || len(rule["groups"].([]any)) != 1 || len(rule["emails"].([]any)) != 1 {
		t.Fatalf("rule: %v", rule)
	}
	_, plain, e := run("ls", sat)
	if e != nil || !strings.Contains(plain, `"circles":["team"]`) {
		t.Fatalf("plain: %q %v", plain, e)
	}
	got, _, e = run("rm", sat, "0", "--json")
	if e != nil || len(got["rules"].([]any)) != 0 {
		t.Fatalf("rm: %v %v", got, e)
	}
	file, e := os.CreateTemp(t.TempDir(), "rules-*.json")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = file.WriteString(`[{"groups":["ops"]}]`)
	_ = file.Close()
	got, _, e = run("set", sat, "--json-rules", file.Name(), "--json")
	if e != nil {
		t.Fatal(e)
	}
	rule = got["rules"].([]any)[0].(map[string]any)
	if len(rule["groups"].([]any)) != 1 || len(rule["circles"].([]any)) != 0 {
		t.Fatalf("set: %v", rule)
	}
	_, _, e = runGrantCommand(t, NewSatelliteCmd(), "admit", "ls", sat, "--master", f.URL(), "--token", other)
	if e == nil || !strings.Contains(e.Error(), "not_owner") {
		t.Fatalf("other owner: %v", e)
	}
}
