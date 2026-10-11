package toolcmd

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRunPeerCapabilityFailsClosedBeforeSelectionOrNetwork(t *testing.T) {
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	var hits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer peer.Close()
	var events int
	oldPublish := publishSelection
	publishSelection = func(room.Event) error { events++; return nil }
	t.Cleanup(func() { publishSelection = oldPublish })

	peerURL := strings.Replace(peer.URL, "http://", "http://owner-secret-token@", 1)
	for _, args := range [][]string{
		{"--capability", "deep-research", "--peer", peerURL, "--", "question"},
		{"vendor-b:research", "--peer", "dragon", "--", "question"},
	} {
		c := newRunCmd()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		err := c.Execute()
		if !errors.Is(err, ErrPeerDispatchUnavailable) {
			t.Fatalf("%v: err = %v, want ErrPeerDispatchUnavailable", args, err)
		}
		if strings.Contains(err.Error(), "owner-secret-token") || strings.Contains(err.Error(), peer.URL[len("http://"):]) {
			t.Fatalf("refusal echoes the peer address: %v", err)
		}
		for _, want := range []string{"7a5f32dbaafa", "53fde6fa2d0d"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not name prerequisite %s", err, want)
			}
		}
	}
	if hits.Load() != 0 || events != 0 {
		t.Fatalf("peer refusal reached the wire (%d requests) or published %d selection events", hits.Load(), events)
	}

	// The local path is unchanged: an empty peer proceeds to measured
	// selection, which still refuses an unmeasured host without seeding.
	c := newRunCmd()
	c.SetArgs([]string{"--capability", "deep-research", "--peer", " ", "--", "question"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err == nil || errors.Is(err, ErrPeerDispatchUnavailable) || !strings.Contains(err.Error(), "no measured provider matrix") {
		t.Fatalf("local selection err = %v", err)
	}
}
