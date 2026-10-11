package cligw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// Sprint 418 S9: X-Bashy-Capability makes THIS door pick the provider from its
// own measurements and run it through the slash path.
func TestCapabilityHeaderRunsDoorMeasuredSelection(t *testing.T) {
	runner := &fakeRunner{res: toolcmd.Result{Outcome: toolcmd.OutcomeSuccess, Text: "the report"}}
	ts := newSlashServer(t, runner)
	var gotCap, gotCaller string
	old := selectMeasured
	selectMeasured = func(_ context.Context, capText, caller string, _ *fleet.Catalog) (toolcmd.Selection, error) {
		gotCap, gotCaller = capText, caller
		if caller == "claude" {
			return toolcmd.Selection{}, errors.New("toolcmd: no measured non-caller provider with remaining quota for capability \"deep-research\"")
		}
		return toolcmd.Selection{Capability: capText, Vendor: "claude", Binding: "warm-four", Command: "review", RemainingQuota: .5, MeasuredSamples: 1}, nil
	}
	t.Cleanup(func() { selectMeasured = old })

	resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "auto", ""),
		map[string]string{CapabilityHeader: "deep-research", CallerHeader: "codex"})
	out := decodeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %v", resp.Status, out)
	}
	if gotCap != "deep-research" || gotCaller != "codex" {
		t.Fatalf("selector saw cap=%q caller=%q", gotCap, gotCaller)
	}
	calls := runner.snapshot()
	if len(calls) != 1 || calls[0].Tool != "claude" || calls[0].Command != "review" || calls[0].Agent != "warm-four" || calls[0].Args != "add a cache layer" {
		t.Fatalf("runner calls %+v", calls)
	}
	tc, _ := out["x_bashy_tool_command"].(map[string]any)
	sel, _ := tc["selection"].(map[string]any)
	if sel["binding"] != "warm-four" || sel["vendor"] != "claude" || sel["capability"] != "deep-research" {
		t.Fatalf("selection not returned: %v", tc)
	}

	for label, tc := range map[string]struct {
		model  string
		hdr    map[string]string
		status int
		want   string
	}{
		"model not auto":     {"warm-four", map[string]string{CapabilityHeader: "deep-research"}, 400, "send model \"auto\""},
		"slash and cap":      {"auto", map[string]string{CapabilityHeader: "deep-research", FilterHeader: "slash=review"}, 400, "not both"},
		"unknown capability": {"auto", map[string]string{CapabilityHeader: "astrology"}, 400, "unknown capability"},
		"unmeasured":         {"auto", map[string]string{CapabilityHeader: "deep-research", CallerHeader: "claude"}, 404, "no measured"},
	} {
		resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, tc.model, ""), tc.hdr)
		out := decodeBody(t, resp)
		if resp.StatusCode != tc.status || !strings.Contains(errMessage(out), tc.want) {
			t.Fatalf("%s: %s %v", label, resp.Status, out)
		}
	}
	if n := len(runner.snapshot()); n != 1 {
		t.Fatalf("refused capability requests ran %d commands", n-1)
	}
}
