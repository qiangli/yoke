package fleet

import (
	"strings"
	"testing"
)

func TestAgentFieldsRoundTripThroughCRUDAndLegacyLoad(t *testing.T) {
	isolatedFleetRoot(t)
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithoutCloudOverlay()}

	if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "helper", "--tool", "codex", "--model", "gpt", "--workdir", "/work/helper", "--permission", PermissionReview, "--notify", "--avatar", "avatar://helper", "--set", "bindings.0.channel=slack", "--set", "bindings.0.sender=alice", "--set", "bindings.0.agent=reviewer"); err != nil {
		t.Fatal(err)
	}
	show, err := runCmd(t, NewAgentsCmd(opts...), "show", "helper", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workdir: /work/helper", "permission: review", "notify: true", "avatar: avatar://helper", "channel: slack", "sender: alice", "agent: reviewer"} {
		if !strings.Contains(show, want) {
			t.Errorf("show missing %q:\n%s", want, show)
		}
	}
	if _, err := runCmd(t, NewAgentsCmd(opts...), "set", "helper", "--permission", PermissionDeny, "--workdir", "/work/locked"); err != nil {
		t.Fatal(err)
	}
	a, ok := New(opts...).Agent("helper")
	if !ok || a.Permission != PermissionDeny || a.Workdir != "/work/locked" || len(a.Bindings) != 1 {
		t.Fatalf("agent after set = %+v, found %v", a, ok)
	}

	legacy, err := ParseAgentFile("old", []byte("name: old\ntool: codex\nmodel: gpt\n"), nil)
	if err != nil || len(legacy.Agents) != 1 || legacy.Agents[0].Permission != PermissionAuto {
		t.Fatalf("legacy record = %+v, err=%v", legacy, err)
	}
}

func TestBindingForMostSpecificWins(t *testing.T) {
	bindings := []AgentBinding{
		{Agent: "default"},
		{Channel: "slack", Agent: "channel"},
		{Channel: "slack", Sender: "alice", Agent: "peer"},
	}
	for _, tt := range []struct{ channel, sender, want string }{
		{"slack", "alice", "peer"},
		{"slack", "bob", "channel"},
		{"discord", "alice", "default"},
	} {
		got, ok := BindingFor(bindings, tt.channel, tt.sender)
		if !ok || got != tt.want {
			t.Errorf("BindingFor(%q, %q) = %q, %v; want %q, true", tt.channel, tt.sender, got, ok, tt.want)
		}
	}
}
