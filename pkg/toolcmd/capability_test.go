package toolcmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

type selectionQuotaAdapter struct{ used float64 }

func (selectionQuotaAdapter) Kind() string { return "selection-quota" }
func (a selectionQuotaAdapter) Collect(_ context.Context, _ llmbudget.SourceConfig, now time.Time) (llmbudget.SourceResult, error) {
	return llmbudget.SourceResult{Status: "ok", Metrics: []llmbudget.Metric{{Name: "quota.used_percent", Value: &a.used, Classification: "actual", Source: "test", ObservedAt: now}}}, nil
}

func TestSelectMeasuredCommandSelectsOtherVendorAndPublishesEvent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_CAPABILITY_DIR", dir)
	tools := t.TempDir()
	t.Setenv("BASHY_TOOLS_PATH", tools)
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "vendor-b"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "vendor-b.yaml"), []byte(`name: vendor-b
kind: cli
cli:
  binary: vendor-b
  launch:
    exec: vendor-b {prompt}
commands:
  - name: research
    slash: /research {args}
    mode: print
    capability: deep-research
`), 0o644); err != nil {
		t.Fatal(err)
	}
	matrix := capability.Matrix{SchemaVersion: "bashy-capability-v1", Agents: map[string]map[capability.Capability]capability.Cell{
		"vendor-b:model-b": {capability.CapDeepResearch: {Quality: .9, Source: capability.SourceHost, Samples: 2}},
	}}
	b, err := json.Marshal(matrix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "matrix.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	g := llmbudget.New(llmbudget.Config{Now: func() time.Time { return now }, Adapters: []llmbudget.Adapter{selectionQuotaAdapter{used: 25}}, Policy: &llmbudget.Policy{Version: 1,
		Bindings: []llmbudget.Binding{{Model: "model-b", Agent: "vendor-b:model-b", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: llmbudget.LaneSubscription}},
		Sources:  []llmbudget.SourceConfig{{ID: "seat", Kind: "selection-quota", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: llmbudget.LaneSubscription, Enabled: true}},
	}})
	restore := llmbudget.SetDefault(g)
	t.Cleanup(restore)
	var event room.Event
	oldPublish := publishSelection
	publishSelection = func(got room.Event) error { event = got; return nil }
	t.Cleanup(func() { publishSelection = oldPublish })
	tool, command, model, selected, err := SelectMeasuredCommand(context.Background(), "deep-research", "vendor-a", fleet.New(fleet.WithRoot(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name != "vendor-b" || command.Name != "research" || model != "model-b" || selected.Vendor != "vendor-b" || selected.RemainingQuota != .75 {
		t.Fatalf("selection = tool=%+v command=%+v model=%q selected=%+v", tool, command, model, selected)
	}
	if err := recordSelection(selected); err != nil {
		t.Fatal(err)
	}
	if event.Type != room.EventUsage || event.Topic != "capability-selection" || event.Actor != "vendor-b" {
		t.Fatalf("event = %+v", event)
	}
}

func TestSelectMeasuredCommandDoesNotSeedMissingMatrix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_CAPABILITY_DIR", dir)
	_, _, _, _, err := SelectMeasuredCommand(context.Background(), "deep-research", "caller", nil)
	if err == nil {
		t.Fatal("SelectMeasuredCommand unexpectedly selected from an absent matrix")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "matrix.json")); !os.IsNotExist(statErr) {
		t.Fatalf("selection seeded matrix: %v", statErr)
	}
}

func writePeers(t *testing.T, mode os.FileMode, peers map[string]PeerDoor) {
	t.Helper()
	dir := filepath.Join(os.Getenv("BASHY_HOME"), "toolcmd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"peers": peers})
	path := filepath.Join(dir, "peers.json")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunPeerCapabilityFailsClosedBeforeNetwork(t *testing.T) {
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	var hits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer peer.Close()
	var events int
	oldPublish := publishSelection
	publishSelection = func(room.Event) error { events++; return nil }
	t.Cleanup(func() { publishSelection = oldPublish })

	const secret = "bpeer_secret_credential"
	peerURL := strings.Replace(peer.URL, "http://", "http://owner-secret-token@", 1)
	run := func(args ...string) error {
		c := newRunCmd()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}
	check := func(label string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", label, err, want)
		}
		for _, leak := range []string{"owner-secret-token", secret, peer.URL[len("http://"):]} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: refusal echoes %q: %v", label, leak, err)
			}
		}
	}
	check("address as peer", run("--capability", "deep-research", "--peer", peerURL, "--", "q"), "not an address")
	check("no peers file", run("--capability", "deep-research", "--peer", "dragon", "--", "q"), "no peers file")
	check("ref instead of capability", run("vendor-b:research", "--peer", "dragon", "--", "q"), "name a capability")

	writePeers(t, 0o644, map[string]PeerDoor{"dragon": {URL: peer.URL, Credential: secret}})
	check("group-readable peers file", run("--capability", "deep-research", "--peer", "dragon", "--", "q"), "must be 0600")

	writePeers(t, 0o600, map[string]PeerDoor{"dragon": {URL: peerURL, Credential: secret}, "bare": {URL: peer.URL}})
	check("unknown peer", run("--capability", "deep-research", "--peer", "other", "--", "q"), "unknown peer")
	check("credential in url", run("--capability", "deep-research", "--peer", "dragon", "--", "q"), "never the url")
	check("no credential", run("--capability", "deep-research", "--peer", "bare", "--", "q"), "no credential")
	check("unknown capability", run("--capability", "nope", "--peer", "bare", "--", "q"), "unknown capability")
	check("async", run("--capability", "deep-research", "--peer", "bare", "--async", "--", "q"), "--async")

	if hits.Load() != 0 || events != 0 {
		t.Fatalf("refusals reached the wire (%d requests) or published %d selection events", hits.Load(), events)
	}

	// The local path is unchanged: an empty peer proceeds to measured
	// selection, which still refuses an unmeasured host without seeding.
	if err := run("--capability", "deep-research", "--peer", " ", "--", "question"); err == nil || !strings.Contains(err.Error(), "no measured provider matrix") {
		t.Fatalf("local selection err = %v", err)
	}
}

// fakePeerDoor answers like a peer door: denied for a wrong credential, else
// the slash reply with the peer-side selection.
func fakePeerDoor(t *testing.T, cred string, sel *Selection, tool string) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+cred {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"message":"peer credential is valid but this door does not share with that peer (share: deny)"}}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get(CapabilityHeader) != "deep-research" || r.Header.Get(CallerHeader) != "codex" {
			t.Errorf("door got %s cap=%q caller=%q", r.URL.Path, r.Header.Get(CapabilityHeader), r.Header.Get(CallerHeader))
		}
		var body struct {
			Model    string
			Messages []struct{ Role, Content string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "auto" || len(body.Messages) != 1 || body.Messages[0].Content != "why ULA" {
			t.Errorf("door body %+v", body)
		}
		res := Result{Tool: tool, Command: "deep-research", Outcome: OutcomeSuccess, Text: "peer report [RFC 4193]", Selection: sel}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": res.Text}}}, "x_bashy_tool_command": res})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestRunPeerCapabilityRoundTripRecordsPeerSelection(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var got []room.Event
	oldPublish := publishSelection
	publishSelection = func(e room.Event) error { got = append(got, e); return nil }
	t.Cleanup(func() { publishSelection = oldPublish })

	sel := &Selection{Capability: "deep-research", Vendor: "claude", Binding: "claude:opus5.5", Command: "deep-research", RemainingQuota: .5, MeasuredSamples: 1}
	door, _ := fakePeerDoor(t, "bpeer_ok", sel, "claude")
	writePeers(t, 0o600, map[string]PeerDoor{"dragon": {URL: door.URL, Credential: "bpeer_ok"}, "stale": {URL: door.URL, Credential: "bpeer_revoked"}})

	var out strings.Builder
	c := newRunCmd()
	c.SetArgs([]string{"--capability", "deep-research", "--peer", "dragon", "--caller", "codex", "--json", "--", "why", "ULA"})
	c.SetOut(&out)
	c.SetErr(io.Discard)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
		t.Fatal(err)
	}
	if res.Text != "peer report [RFC 4193]" || res.Selection == nil || res.Selection.Peer != "dragon" || res.Selection.Vendor != "claude" {
		t.Fatalf("result %+v", res)
	}
	if len(got) != 1 || got[0].Topic != "capability-selection" || !strings.Contains(got[0].Body, "peer=dragon") || !strings.Contains(got[0].Body, "binding=claude:opus5.5") || got[0].Actor != "claude" {
		t.Fatalf("selection events %+v", got)
	}

	// A denied/revoked credential is the door's refusal, with no event and no echo.
	_, err := DispatchPeerCapability(t.Context(), "stale", "deep-research", "codex", "why ULA", 0)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "bpeer_revoked") || len(got) != 1 {
		t.Fatalf("denied dispatch err = %v events=%d", err, len(got))
	}
}

func TestPeerReplyMustBeAttributedToAnotherVendor(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var events int
	oldPublish := publishSelection
	publishSelection = func(room.Event) error { events++; return nil }
	t.Cleanup(func() { publishSelection = oldPublish })
	for label, tc := range map[string]struct {
		sel  *Selection
		tool string
		want string
	}{
		"unattributed":  {nil, "claude", "no measured selection"},
		"caller vendor": {&Selection{Capability: "deep-research", Vendor: "codex", Binding: "codex:gpt", Command: "x"}, "codex", "caller's own vendor"},
		"mismatch":      {&Selection{Capability: "deep-research", Vendor: "claude", Binding: "claude:opus", Command: "x"}, "gemini", "served the command"},
	} {
		door, _ := fakePeerDoor(t, "bpeer_ok", tc.sel, tc.tool)
		writePeers(t, 0o600, map[string]PeerDoor{"dragon": {URL: door.URL, Credential: "bpeer_ok"}})
		if _, err := DispatchPeerCapability(t.Context(), "dragon", "deep-research", "codex", "why ULA", 0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", label, err)
		}
	}
	if events != 0 {
		t.Fatalf("unaccepted replies published %d events", events)
	}
}
