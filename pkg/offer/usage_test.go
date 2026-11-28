package offer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/sharedfiles"
	"github.com/qiangli/yoke/pkg/toolcmd"
	"github.com/spf13/cobra"
)

func TestRemoteCallAndUsageReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	state := filepath.Join(home, "broker")
	peers := filepath.Join(state, "peers.json")
	credential, err := broker.MintPeerCredential(peers, "reporter", broker.ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetPeerCommands(peers, "reporter", []string{"sample"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	def := ToolDefinition{Name: "sample", Class: "tool", Unit: "call", Input: map[string]any{"type": "object"}, Tags: []string{"demo"}}
	digest, err := def.Digest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := broker.OffersDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SaveOffer(dir, broker.OfferRecord{Name: def.Name, Definition: raw, Digest: digest, SSOSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	b, err := broker.New(t.Context(), broker.Options{Token: "owner-token", PeersFile: peers, StateDir: state, OffersDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	broker.RegisterOfferRunner(func(context.Context, broker.OfferRecord, map[string]any) (json.RawMessage, any, error) {
		return json.RawMessage(`{"ok":true}`), nil, nil
	})
	t.Cleanup(func() { broker.RegisterOfferRunner(runOffer) })
	server := httptest.NewServer(b)
	defer server.Close()
	if err := toolcmd.SavePeer("host", toolcmd.PeerDoor{URL: server.URL, Credential: credential}); err != nil {
		t.Fatal(err)
	}
	execute := func(cmd *cobra.Command, args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %s, %v", args, out.String(), err)
		}
		return out.Bytes()
	}
	body := execute(newRemoteCallCmd(), "host", "sample", "--json")
	var reply struct{ Usage broker.OfferUsage }
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	path, err := broker.RemoteUsagePath()
	if err != nil {
		t.Fatal(err)
	}
	caller, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var remote struct {
		Host string
		broker.OfferUsage
	}
	if err := json.Unmarshal(caller, &remote); err != nil {
		t.Fatal(err)
	}
	if remote.Host != "host" || remote.OfferUsage != reply.Usage || remote.Units != 1 || remote.Digest != digest || strings.Contains(string(caller), "tags") {
		t.Fatalf("caller row %s, door usage %+v", caller, reply.Usage)
	}
	audit, err := os.ReadFile(filepath.Join(state, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(audit), []byte("\n"))
	var rec broker.Record
	if err := json.Unmarshal(lines[len(lines)-1], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Tool != remote.Tool || rec.OfferClass != remote.Class || rec.Unit != remote.Unit || rec.Units != remote.Units || rec.Digest != remote.Digest || rec.Principal != "peer:reporter" {
		t.Fatalf("door %+v, caller %+v", rec, remote)
	}

	// Historical tag lookup survives removal of the live offer.
	if err := os.Remove(filepath.Join(dir, def.Name+".json")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remote  bool
		by, key string
	}{
		{false, "member", "peer:reporter"}, {true, "host", "host"}, {false, "tool", "sample"}, {true, "tool", "sample"},
	} {
		cmd := NewOfferCmd()
		if tc.remote {
			cmd = &cobra.Command{Use: "remote"}
			cmd.AddCommand(RemoteSubcommands()...)
		}
		result := execute(cmd, "usage", "--by", tc.by, "--tag", "demo", "--json")
		var rows []map[string]any
		if err := json.Unmarshal(result, &rows); err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{tc.by: tc.key, "calls": float64(1), "units": map[string]any{"call": float64(1)}}}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("report %s want %v", result, want)
		}
		if result := execute(cmd, "usage", "--by", tc.by, "--tag", "absent", "--json"); strings.TrimSpace(string(result)) != "[]" {
			t.Fatalf("unmatched tag: %s", result)
		}
	}

	// A legacy line keeps its call and known tokens without an invented tool.
	f, err := os.OpenFile(filepath.Join(state, "audit.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("{\"principal\":\"owner\",\"prompt_tokens\":3,\"completion_tokens\":2}\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	result := execute(NewOfferCmd(), "usage", "--by", "member", "--json")
	var legacy []struct {
		Member string
		Calls  int
		Units  map[string]float64
	}
	if err = json.Unmarshal(result, &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 2 || legacy[0].Member != "owner" || legacy[0].Calls != 1 || legacy[0].Units["token"] != 5 {
		t.Fatalf("legacy rollup %s", result)
	}
}

func TestFilesCallerRecordsInlineAndStream(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dir := filepath.Join(home, "shared")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs.json"), []byte(`{"name":"docs"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "sample"), []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	peers := filepath.Join(home, "peers.json")
	cred, err := broker.MintPeerCredential(peers, "caller", broker.ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = broker.SetPeerCommands(peers, "caller", []string{"files-docs"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := broker.New(t.Context(), broker.Options{Token: "owner", PeersFile: peers})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b)
	defer srv.Close()
	if err = toolcmd.SavePeer("host", toolcmd.PeerDoor{URL: srv.URL, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	cmd := sharedfiles.NewCmd()
	var out bytes.Buffer
	cmd.SetArgs([]string{"cat", "host:docs/sample", "--length", "2"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil || out.String() != "sa" {
		t.Fatalf("inline %q %v", out.String(), err)
	}
	if err := sharedfiles.Fetch(t.Context(), "bashy://host/files-docs/sample", filepath.Join(t.TempDir(), "sample")); err != nil {
		t.Fatal(err)
	}
	cmd = newOfferUsageCmd(true)
	cmd.SetArgs([]string{"--by", "tool", "--json"})
	out.Reset()
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Tool  string
		Calls int
		Units map[string]float64
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Tool != "files-docs" || rows[0].Calls != 2 || rows[0].Units["bytes"] != 8 {
		t.Fatalf("folder report %s, %v", out.String(), err)
	}
}

func TestRemoteUsageResolvesMountedDigest(t *testing.T) {
	door, opts := newMountDoor(t)
	if out, err := runMount(t, newRemoteAddCmd(opts), "host", "sample"); err != nil {
		t.Fatalf("mount %s: %v", out, err)
	}
	if err := broker.AppendRemoteUsage("host", broker.OfferUsage{Tool: "sample", Class: "tool", Unit: "call", Units: 1, Digest: door.digest}); err != nil {
		t.Fatal(err)
	}
	out, err := runMount(t, newOfferUsageCmd(true, opts...), "--by", "tool", "--json", "--tag", "alpha")
	if err != nil || !strings.Contains(out, `"tool":"sample"`) {
		t.Fatalf("mounted tag: %s %v", out, err)
	}
}
