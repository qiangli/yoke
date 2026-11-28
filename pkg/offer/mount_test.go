package offer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/toolcmd"
	"github.com/spf13/cobra"
)

// mountDoor is a peer door serving one offer whose digest the test can move.
type mountDoor struct {
	digest string
	posts  []map[string]any
	pinned []string
}

func newMountDoor(t *testing.T) (*mountDoor, []fleet.Option) {
	t.Helper()
	door := &mountDoor{digest: "digest-1"}
	def := ToolDefinition{Name: "sample", Class: "tool", Description: "Count things\nmore", Unit: "call", Effects: []string{"read"}, Tags: []string{"alpha", "beta"}, Input: map[string]any{
		"type": "object", "required": []string{"count"}, "additionalProperties": false,
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
			"mode":  map[string]any{"type": "string", "default": "fast", "enum": []string{"fast", "slow"}},
			"dry":   map[string]any{"type": "boolean"},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/offers/sample" || r.Header.Get("Authorization") != "Bearer bpeer_test" {
			http.Error(w, "no", 403)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"definition": def, "digest": door.digest})
			return
		}
		door.pinned = append(door.pinned, r.Header.Get(broker.OfferDigestHeader))
		var body struct {
			Args map[string]any `json:"args"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		door.posts = append(door.posts, body.Args)
		_, _ = w.Write([]byte(`{"result":{"ok":true},"usage":{"calls":1}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_HOME", t.TempDir())
	if err := toolcmd.SavePeer("host", toolcmd.PeerDoor{URL: srv.URL, Credential: "bpeer_test"}); err != nil {
		t.Fatal(err)
	}
	reserved := func(name string) (string, bool) { return "the shipped verb", name == "shipped" }
	return door, []fleet.Option{fleet.WithRoot(t.TempDir()), fleet.WithReservedNames(reserved)}
}

func runMount(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// callMounted runs a mounted record the way the shell does: the record's
// env for the child, its exec template with the user args spliced in.
func callMounted(t *testing.T, rec fleet.Command, args ...string) (string, error) {
	t.Helper()
	for _, kv := range rec.Env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	argv := rec.Argv("bashy", "bashy", args)
	if len(argv) < 3 || argv[1] != "remote" || argv[2] != "call" {
		t.Fatalf("argv %v", argv)
	}
	return runMount(t, newRemoteCallCmd(), argv[3:]...)
}

func TestRemoteAddWritesTypedRecordListedAsMCPTool(t *testing.T) {
	door, opts := newMountDoor(t)
	out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample", "--json")
	if err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	var added Mount
	if err := json.Unmarshal([]byte(out), &added); err != nil {
		t.Fatal(err)
	}
	if added.Name != "sample" || added.Host != "host" || added.Offer != "sample" || added.Class != "tool" || added.Unit != "call" || added.Digest != "digest-1" || strings.Join(added.Tags, ",") != "alpha,beta" {
		t.Fatalf("mount %+v", added)
	}
	rec, ok := fleet.New(opts...).Command("sample")
	if !ok {
		t.Fatal("no registered record")
	}
	if err := rec.Validate(nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.Exec, " "); got != "bashy remote call host sample {args}" {
		t.Fatalf("exec %q", got)
	}
	if strings.Join(rec.Effects, ",") != "net,read" || rec.Synopsis != "Count things" {
		t.Fatalf("effects %v synopsis %q", rec.Effects, rec.Synopsis)
	}
	if rec.Args == nil || len(rec.Args.Flags) != 3 {
		t.Fatalf("args %+v", rec.Args)
	}
	for _, f := range rec.Args.Flags {
		switch f.Name {
		case "count":
			if f.Type != fleet.ArgTypeInt || !f.Required {
				t.Errorf("count %+v", f)
			}
		case "mode":
			if f.Default != "fast" || strings.Join(f.Enum, ",") != "fast,slow" || f.Required {
				t.Errorf("mode %+v", f)
			}
		case "dry":
			if f.Type != fleet.ArgTypeBool {
				t.Errorf("dry %+v", f)
			}
		}
	}

	// The record is a typed tool in the MCP tool list, and a call through it
	// reaches the door with the pinned digest.
	schema := rec.Args.ArgumentSchema()
	registered := mcp.RegisteredCommand{Name: rec.Name, Synopsis: rec.Synopsis, Effects: rec.Effects, OS: rec.OS, Schema: &schema, Run: func(_ context.Context, argv []string, _, _ string) (string, string, int) {
		out, err := callMounted(t, rec, argv...)
		if err != nil {
			return out, err.Error(), 1
		}
		return out, "", 0
	}}
	srv := mcp.NewServerWithOptions("mounted", "test", mcp.Options{
		Policy:     &mcp.Policy{Allow: map[string]bool{"net": true, "read": true}, Audit: func(mcp.Record) {}},
		Registered: func() []mcp.RegisteredCommand { return []mcp.RegisteredCommand{registered} },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "mount-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range listed.Tools {
		if d.Name != "sample" {
			continue
		}
		found = true
		props := d.InputSchema.(map[string]any)["properties"].(map[string]any)
		if props["count"].(map[string]any)["type"] != "integer" || props["dry"].(map[string]any)["type"] != "boolean" || props["mode"].(map[string]any)["default"] != "fast" || props["args"] != nil {
			t.Fatalf("descriptor %+v", d.InputSchema)
		}
	}
	if !found {
		t.Fatal("mounted tool missing from the MCP tool list")
	}
	result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "sample", Arguments: map[string]any{"count": 3, "dry": true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool call failed: %+v", result.Content)
	}
	if len(door.posts) != 1 || door.posts[0]["count"] != float64(3) || door.posts[0]["dry"] != true || door.posts[0]["mode"] != "fast" {
		t.Fatalf("door calls %v", door.posts)
	}
	if door.pinned[0] != "digest-1" {
		t.Fatalf("pinned digest header %q", door.pinned[0])
	}
}

func TestRemoteCallRefusesChangedDigestUntilReAdded(t *testing.T) {
	door, opts := newMountDoor(t)
	if out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample"); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	rec, _ := fleet.New(opts...).Command("sample")
	if out, err := callMounted(t, rec, "--count=1"); err != nil {
		t.Fatalf("call: %v: %s", err, out)
	}
	door.digest = "digest-2"
	_, err := callMounted(t, rec, "--count", "1")
	if err == nil || !strings.Contains(err.Error(), "bashy remote add host sample") {
		t.Fatalf("changed digest: %v", err)
	}
	if len(door.posts) != 1 {
		t.Fatalf("a refused call reached the door: %v", door.posts)
	}
	// An unmounted direct call carries no pin and is not refused.
	t.Setenv(mountEnvHost, "")
	if out, err := runMount(t, newRemoteCallCmd(), "host", "sample", "--count", "1"); err != nil {
		t.Fatalf("direct call: %v: %s", err, out)
	}
	if door.pinned[len(door.pinned)-1] != "" {
		t.Fatalf("direct call sent a pin: %v", door.pinned)
	}
	// Re-adding the same mount accepts the new definition.
	if out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample"); err != nil {
		t.Fatalf("re-add: %v: %s", err, out)
	}
	rec, _ = fleet.New(opts...).Command("sample")
	if out, err := callMounted(t, rec, "--count", "1"); err != nil {
		t.Fatalf("call after re-add: %v: %s", err, out)
	}
	if door.pinned[len(door.pinned)-1] != "digest-2" {
		t.Fatalf("pins %v", door.pinned)
	}
}

func TestRemoteAddRenameShadowListRemove(t *testing.T) {
	_, opts := newMountDoor(t)
	cat := fleet.New(opts...)
	if err := cat.SaveCommand(fleet.Command{Name: "sample", Script: "echo hi", Effects: []string{"pure"}}); err != nil {
		t.Fatal(err)
	}
	// NAME shadows an existing registered command.
	if out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample"); err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("shadowing add: %v: %s", err, out)
	}
	// A name the shell ships, and a word `commands` keeps, are refused too.
	for _, local := range []string{"shipped", "list"} {
		if out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample", "--as", local); err == nil {
			t.Fatalf("reserved name %s accepted: %s", local, out)
		}
		if _, ok := cat.Command(local); ok {
			t.Fatalf("reserved name %s was written", local)
		}
	}
	if out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample", "--as", "far-sample"); err != nil {
		t.Fatalf("add --as: %v: %s", err, out)
	}
	rec, ok := cat.Command("far-sample")
	if !ok || strings.Join(rec.Exec, " ") != "bashy remote call host sample {args}" {
		t.Fatalf("renamed record %+v", rec)
	}
	if plain, _ := cat.Command("sample"); plain.Script != "echo hi" {
		t.Fatalf("the shadowed command was overwritten: %+v", plain)
	}

	for tag, want := range map[string]int{"": 1, "alpha": 1, "gamma": 0} {
		args := []string{"--json"}
		if tag != "" {
			args = append(args, "--tag", tag)
		}
		out, err := runMount(t, newRemoteListCmd(opts), args...)
		if err != nil {
			t.Fatal(err)
		}
		var rows []Mount
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if len(rows) != want {
			t.Fatalf("list --tag %q: %+v", tag, rows)
		}
		if want == 1 && (rows[0].Name != "far-sample" || rows[0].Host != "host" || rows[0].Class != "tool" || rows[0].Unit != "call" || rows[0].Digest != "digest-1") {
			t.Fatalf("row %+v", rows[0])
		}
	}
	if out, _ := runMount(t, newRemoteListCmd(opts)); !strings.Contains(out, "far-sample\thost\ttool\tcall\tdigest-1\talpha,beta") {
		t.Fatalf("list text %q", out)
	}

	// remove deletes a mount and nothing else.
	if _, err := runMount(t, newRemoteRemoveCmd(opts), "sample"); err == nil {
		t.Fatal("remove deleted a command that is not a mount")
	}
	if out, err := runMount(t, newRemoteRemoveCmd(opts), "far-sample", "--json"); err != nil || !strings.Contains(out, `"removed"`) {
		t.Fatalf("remove: %v: %s", err, out)
	}
	if _, ok := cat.Command("far-sample"); ok {
		t.Fatal("record survived remove")
	}
	if _, ok := cat.Command("sample"); !ok {
		t.Fatal("remove took the unrelated command")
	}
}
