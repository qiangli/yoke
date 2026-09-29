package cligw

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// Sprint #324 S5: the slash=<name> filter key. Nothing here runs a real
// tool command: the runner is the runToolCommand seam, replaced by a fake.

func TestParseFilterMergeMatchSlash(t *testing.T) {
	f, err := ParseFilter("slash=plan, tool=claude")
	if err != nil {
		t.Fatal(err)
	}
	if f.Slash != "plan" || f.Tool != "claude" {
		t.Fatalf("ParseFilter = %+v", f)
	}
	// The spelling a human types is accepted and normalised.
	if f, _ := ParseFilter("slash=/review"); f.Slash != "review" {
		t.Fatalf("slash=/review parsed as %q", f.Slash)
	}
	if _, err := ParseFilter("slash="); err == nil {
		t.Fatal("empty slash value accepted")
	}
	merged := Filter{Provider: "anthropic"}.Merge(Filter{Slash: "plan"})
	if merged.Slash != "plan" || merged.Provider != "anthropic" {
		t.Fatalf("Merge = %+v", merged)
	}
	if kept := (Filter{Slash: "plan"}).Merge(Filter{}); kept.Slash != "plan" {
		t.Fatalf("an empty request field must not clear slash: %+v", kept)
	}
	planner := Agent{Name: "a", Commands: []string{"plan", "review"}}
	plain := Agent{Name: "b"}
	if !(Filter{Slash: "plan"}).Match(planner) || (Filter{Slash: "plan"}).Match(plain) {
		t.Fatal("Match must keep exactly the agents whose tool declares the command")
	}
	if (Filter{Slash: "deep-research"}).Match(planner) {
		t.Fatal("Match kept an agent for an undeclared command")
	}
	if !(Filter{}).Match(plain) {
		t.Fatal("an empty slash must not constrain")
	}
}

func TestPolicyRefusesSlashDefault(t *testing.T) {
	p := DefaultPolicy()
	p.Filter.Slash = "plan"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "per-request") {
		t.Fatalf("policy filter.slash accepted: %v", err)
	}
}

// installSlashFleet: two L4 agents on two tools — "claude" declares plan and
// review, "fakecold" declares review only — plus an L2 claude agent. A third
// tool with no launchable agent declares deep-research, so it is in the
// declared vocabulary but has no candidate.
func installSlashFleet(t *testing.T) *FleetCatalog {
	t.Helper()
	t.Setenv("CLIGW_TEST_HELPER", "1")
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	launch := fleet.ToolLaunch{Exec: fmt.Sprintf("%s -test.run=TestCLIHelper -- cold {model} {prompt}", os.Args[0])}
	tools := []fleet.Tool{
		{Name: "claude", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch},
			Commands: []fleet.ToolCommand{
				{Name: "plan", Slash: "/plan {args}", Mode: fleet.ToolCommandPrint, Timeout: "2s"},
				{Name: "review", Slash: "/review {args}", Mode: fleet.ToolCommandPrint},
			}},
		{Name: "fakecold", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch},
			Commands: []fleet.ToolCommand{{Name: "review", Slash: "/review {args}", Mode: fleet.ToolCommandPrint}}},
		{Name: "researcher", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0]},
			Commands: []fleet.ToolCommand{{Name: "deep-research", Slash: "/deep-research {args}", Mode: fleet.ToolCommandPrint}}},
	}
	for _, tool := range tools {
		if err := cat.SaveTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []fleet.Model{
		{Name: "sonnet-x", Band: 4, BandSource: fleet.BandMeasured, Kind: fleet.ModelKindSubscription, Provider: "anthropic", Domain: []string{"coding"}},
		{Name: "gpt-x", Band: 4, BandSource: fleet.BandMeasured, Kind: fleet.ModelKindSubscription, Provider: "openai", Domain: []string{"coding"}},
		{Name: "tiny", Band: 2, Kind: fleet.ModelKindLocal, Provider: "local", Domain: []string{"general"}},
	} {
		if err := cat.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []fleet.Agent{
		{Name: "warm-four", Tool: "claude", Model: "sonnet-x"},
		{Name: "cold-four", Tool: "fakecold", Model: "gpt-x"},
		{Name: "small-two", Tool: "claude", Model: "tiny"},
		{Name: "researcher-four", Tool: "researcher", Model: "gpt-x"},
	} {
		if err := cat.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })
	return NewFleetCatalog(cat)
}

// fakeRunner replaces runToolCommand for one test.
type fakeRunner struct {
	mu    sync.Mutex
	calls []fakeCall
	res   toolcmd.Result
	err   error
	delay time.Duration
}

type fakeCall struct {
	Tool, Command, Args, Agent string
	Deadline                   time.Duration
}

func (f *fakeRunner) run(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts toolcmd.Options) (toolcmd.Result, error) {
	dl, _ := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Tool: tool.Name, Command: cmd.Name, Args: args, Agent: opts.Agent, Deadline: time.Until(dl)})
	res, err, delay := f.res, f.err, f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return toolcmd.Result{Outcome: toolcmd.OutcomeCancelled}, ctx.Err()
		}
	}
	res.Tool, res.Command = tool.Name, cmd.Name
	return res, err
}

func (f *fakeRunner) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func newSlashServer(t *testing.T, runner *fakeRunner) *testServer {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	old := runToolCommand
	runToolCommand = runner.run
	t.Cleanup(func() { runToolCommand = old })
	catalog := installSlashFleet(t)
	quota := &fakeQuota{headroom: map[string]float64{"sonnet-x": .8, "gpt-x": .2}, refused: map[string]string{}}
	server, err := NewServer(ServerOptions{
		Catalog: catalog, Quota: quota, Breaker: sched.NewBreaker(),
		Pool: PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 1, MaxWorkers: 2, IdleTTL: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &testServer{Server: server, http: httpServer, quota: quota}
}

func (ts *testServer) doHeaders(t *testing.T, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.Token())
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s: %v: %s", resp.Status, err, data)
	}
	return out
}

func errMessage(out map[string]any) string {
	switch e := out["error"].(type) {
	case string:
		return e
	case map[string]any:
		m, _ := e["message"].(string)
		return m
	}
	return ""
}

const slashChat = `{"model":%q,"messages":[{"role":"user","content":"earlier"},{"role":"assistant","content":"ok"},{"role":"user","content":"add a cache layer"}]%s}`

func TestSlashModelsListsCapableAgents(t *testing.T) {
	ts := newSlashServer(t, &fakeRunner{})
	ids := func(resp *http.Response) map[string]ModelEntry {
		t.Helper()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %s", resp.Status)
		}
		var list ModelListResponse
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		out := map[string]ModelEntry{}
		for _, e := range list.Data {
			out[e.ID] = e
		}
		return out
	}
	for name, resp := range map[string]*http.Response{
		"query":  ts.doHeaders(t, http.MethodGet, "/v1/models?slash=plan", "", nil),
		"header": ts.doHeaders(t, http.MethodGet, "/v1/models", "", map[string]string{FilterHeader: "slash=plan"}),
	} {
		got := ids(resp)
		if _, ok := got["warm-four"]; !ok {
			t.Fatalf("%s: warm-four (claude declares plan) missing: %v", name, got)
		}
		if _, ok := got["small-two"]; !ok {
			t.Fatalf("%s: small-two missing", name)
		}
		if _, ok := got["cold-four"]; ok {
			t.Fatalf("%s: cold-four (fakecold declares no plan) listed", name)
		}
		if e := got["warm-four"]; strings.Join(e.XCommands, ",") != "plan,review" {
			t.Fatalf("%s: x_commands = %v", name, e.XCommands)
		}
	}
	all := ids(ts.doHeaders(t, http.MethodGet, "/v1/models?slash=review", "", nil))
	if _, ok := all["cold-four"]; !ok {
		t.Fatal("slash=review must list fakecold's agent too")
	}

	bad := ts.doHeaders(t, http.MethodGet, "/v1/models?slash=nope", "", nil)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown slash listing = %s, want 400", bad.Status)
	}
	if msg := errMessage(decodeBody(t, bad)); !strings.Contains(msg, "deep-research, plan, review") {
		t.Fatalf("400 must list declared names: %q", msg)
	}
}

func TestSlashUnknownNameIs400AndNoCandidateIs404(t *testing.T) {
	runner := &fakeRunner{}
	ts := newSlashServer(t, runner)

	resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", ""), map[string]string{FilterHeader: "slash=nope"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown slash = %s, want 400", resp.Status)
	}
	out := decodeBody(t, resp)
	if msg := errMessage(out); !strings.Contains(msg, `"nope"`) || !strings.Contains(msg, "deep-research, plan, review") {
		t.Fatalf("400 message = %q", msg)
	}
	if typ := out["error"].(map[string]any)["type"]; typ != "tool_command_unknown" {
		t.Fatalf("400 type = %v", typ)
	}

	// cold-four's tool declares review only.
	resp = ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "cold-four", ""), map[string]string{FilterHeader: "slash=plan"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no candidate = %s, want 404", resp.Status)
	}
	if msg := errMessage(decodeBody(t, resp)); !strings.Contains(msg, "slash=plan") || !strings.Contains(msg, "claude") {
		t.Fatalf("404 must name the unmet requirement and who declares it: %q", msg)
	}
	// deep-research is declared, but only by a tool with no launchable agent.
	resp = ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", ""), map[string]string{FilterHeader: "slash=deep-research"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("declared-but-unlaunchable = %s, want 404", resp.Status)
	}
	_ = resp.Body.Close()
	if n := len(runner.snapshot()); n != 0 {
		t.Fatalf("runner called %d times on refused requests", n)
	}
}

func TestSlashExecutesOnRoutedAgentOpenAI(t *testing.T) {
	runner := &fakeRunner{res: toolcmd.Result{Mode: "print", Outcome: toolcmd.OutcomeSuccess, Text: "1. add cache\n2. test it", Artifacts: []string{"PLAN.md"}}}
	ts := newSlashServer(t, runner)

	resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", ""), map[string]string{FilterHeader: "slash=plan"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("slash plan = %s: %v", resp.Status, decodeBody(t, resp))
	}
	if got := resp.Header.Get(ToolCommandHeader); got != "claude:plan" {
		t.Fatalf("%s = %q", ToolCommandHeader, got)
	}
	if resp.Header.Get(sched.JobIDHeader) == "" {
		t.Fatalf("missing %s", sched.JobIDHeader)
	}
	if !strings.Contains(resp.Header.Get(RoutedHeader), "agent=warm-four") {
		t.Fatalf("routed = %q", resp.Header.Get(RoutedHeader))
	}
	out := decodeBody(t, resp)
	if out["object"] != "chat.completion" || out["model"] != "L4" {
		t.Fatalf("completion envelope = %v", out)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if content := msg["content"].(string); !strings.Contains(content, "1. add cache") || !strings.Contains(content, "- PLAN.md") {
		t.Fatalf("content = %q", content)
	}
	env := out["x_bashy_tool_command"].(map[string]any)
	if env["outcome"] != "success" || env["tool"] != "claude" || env["command"] != "plan" {
		t.Fatalf("x_bashy_tool_command = %v", env)
	}
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("runner calls = %d", len(calls))
	}
	c := calls[0]
	if c.Tool != "claude" || c.Command != "plan" || c.Agent != "warm-four" || c.Args != "add a cache layer" {
		t.Fatalf("runner call = %+v (args must be the LAST user message)", c)
	}
	// Bounded by the command's declared timeout (2s), not the 10 min request bound.
	if c.Deadline <= 0 || c.Deadline > 2*time.Second {
		t.Fatalf("runner deadline = %v, want <= the command's 2s timeout", c.Deadline)
	}
	// A slash request takes no completion worker: no pool was created.
	if pools := ts.Health().Autoscale.Pools; len(pools) != 0 {
		t.Fatalf("slash request created pools: %+v", pools)
	}
}

func TestSlashExecutesAnthropicMessage(t *testing.T) {
	runner := &fakeRunner{res: toolcmd.Result{Outcome: toolcmd.OutcomeSuccess, Text: "the plan"}}
	ts := newSlashServer(t, runner)
	body := `{"model":"warm-four","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"plan the migration"}]}]}`
	for _, path := range []string{"/v1/messages", "/anthropic/v1/messages"} {
		resp := ts.doHeaders(t, http.MethodPost, path, body, map[string]string{FilterHeader: "slash=plan"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %s: %v", path, resp.Status, decodeBody(t, resp))
		}
		out := decodeBody(t, resp)
		if out["type"] != "message" || out["role"] != "assistant" {
			t.Fatalf("%s: not an Anthropic message: %v", path, out)
		}
		block := out["content"].([]any)[0].(map[string]any)
		if block["type"] != "text" || block["text"] != "the plan" {
			t.Fatalf("%s: content = %v", path, out["content"])
		}
		if out["x_bashy_tool_command"] == nil {
			t.Fatalf("%s: missing envelope", path)
		}
	}
	if c := runner.snapshot(); c[0].Args != "plan the migration" {
		t.Fatalf("anthropic args = %q", c[0].Args)
	}
}

func TestSlashOutcomeMapping(t *testing.T) {
	cases := []struct {
		name   string
		res    toolcmd.Result
		err    error
		status int
		typ    string
		hint   bool
	}{
		{"launch-guard", toolcmd.Result{Outcome: toolcmd.OutcomeError, Refusal: toolcmd.RefusalLaunchGuard, Hint: "run it contained"},
			&toolcmd.RefusalError{Kind: toolcmd.RefusalLaunchGuard, Hint: "run it contained", Err: errors.New("agent launch: refusing to launch")},
			http.StatusForbidden, "tool_command_refused", true},
		{"agent-live", toolcmd.Result{Outcome: toolcmd.OutcomeError, Refusal: toolcmd.RefusalAgentLive, Hint: "clone it"},
			&toolcmd.RefusalError{Kind: toolcmd.RefusalAgentLive, Hint: "clone it", Err: errors.New("is already live")},
			http.StatusConflict, "tool_command_refused", true},
		{"unavailable", toolcmd.Result{Outcome: toolcmd.OutcomeUnavailable, Text: "/plan isn't available in this environment."},
			toolcmd.ErrUnavailable, http.StatusUnprocessableEntity, "tool_command_unavailable", false},
		{"timeout", toolcmd.Result{Outcome: toolcmd.OutcomeTimeout, Error: "deadline"},
			context.DeadlineExceeded, http.StatusGatewayTimeout, "tool_command_timeout", false},
		{"failed", toolcmd.Result{Outcome: toolcmd.OutcomeError, Error: "exit 2"},
			errors.New("exit 2"), http.StatusBadGateway, "tool_command_failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newSlashServer(t, &fakeRunner{res: tc.res, err: tc.err})
			resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", ""), map[string]string{FilterHeader: "slash=plan"})
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %s, want %d", resp.Status, tc.status)
			}
			e := decodeBody(t, resp)["error"].(map[string]any)
			if e["type"] != tc.typ || e["command"] != "plan" || e["tool"] != "claude" || e["message"] == "" {
				t.Fatalf("error body = %v", e)
			}
			if tc.hint && e["hint"] == nil {
				t.Fatalf("refusal lost its hint: %v", e)
			}
			if tc.name == "unavailable" && !strings.Contains(e["message"].(string), "isn't available") {
				t.Fatalf("unavailable must carry the reason: %v", e)
			}
			// The Anthropic surface reports the same status in its own envelope.
			resp = ts.doHeaders(t, http.MethodPost, "/v1/messages",
				`{"model":"L4","max_tokens":8,"messages":[{"role":"user","content":"x"}]}`, map[string]string{FilterHeader: "slash=plan"})
			if resp.StatusCode != tc.status {
				t.Fatalf("anthropic status = %s, want %d", resp.Status, tc.status)
			}
			out := decodeBody(t, resp)
			if out["type"] != "error" || out["error"].(map[string]any)["bashy_type"] != tc.typ {
				t.Fatalf("anthropic error = %v", out)
			}
		})
	}
}

func readSSE(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	return b.String()
}

func TestSlashStreamKeepaliveThenCompletion(t *testing.T) {
	old := slashKeepalive
	slashKeepalive = 10 * time.Millisecond
	t.Cleanup(func() { slashKeepalive = old })
	runner := &fakeRunner{res: toolcmd.Result{Outcome: toolcmd.OutcomeSuccess, Text: "streamed plan"}, delay: 80 * time.Millisecond}
	ts := newSlashServer(t, runner)

	resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", `,"stream":true`), map[string]string{FilterHeader: "slash=plan"})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream = %s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get(sched.JobIDHeader) == "" {
		t.Fatal("stream lost the job id header")
	}
	sse := readSSE(t, resp)
	if !strings.Contains(sse, ": keepalive") || !strings.Contains(sse, "streamed plan") || !strings.Contains(sse, "data: [DONE]") {
		t.Fatalf("sse = %q", sse)
	}

	resp = ts.doHeaders(t, http.MethodPost, "/v1/messages",
		`{"model":"L4","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"x"}]}`, map[string]string{FilterHeader: "slash=plan"})
	sse = readSSE(t, resp)
	for _, want := range []string{"event: ping", "event: message_start", "streamed plan", "event: message_stop"} {
		if !strings.Contains(sse, want) {
			t.Fatalf("anthropic sse missing %q: %q", want, sse)
		}
	}
}

func TestSlashStreamFastRefusalKeepsStatusAndLateFailureIsInBand(t *testing.T) {
	old := slashKeepalive
	slashKeepalive = time.Hour
	t.Cleanup(func() { slashKeepalive = old })
	ts := newSlashServer(t, &fakeRunner{
		res: toolcmd.Result{Refusal: toolcmd.RefusalAgentLive, Hint: "clone it", Outcome: toolcmd.OutcomeError},
		err: &toolcmd.RefusalError{Kind: toolcmd.RefusalAgentLive, Hint: "clone it", Err: errors.New("is already live")},
	})
	resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", `,"stream":true`), map[string]string{FilterHeader: "slash=plan"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("fast refusal on a stream = %s, want 409", resp.Status)
	}
	_ = resp.Body.Close()

	slashKeepalive = 10 * time.Millisecond
	ts = newSlashServer(t, &fakeRunner{res: toolcmd.Result{Outcome: toolcmd.OutcomeTimeout}, err: context.DeadlineExceeded, delay: 60 * time.Millisecond})
	resp = ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", `,"stream":true`), map[string]string{FilterHeader: "slash=plan"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("committed stream = %s", resp.Status)
	}
	if sse := readSSE(t, resp); !strings.Contains(sse, "tool_command_timeout") || !strings.Contains(sse, "[DONE]") {
		t.Fatalf("late failure not reported in-band: %q", sse)
	}
}

func TestOrdinaryRequestNeverRunsToolCommand(t *testing.T) {
	runner := &fakeRunner{}
	ts := newSlashServer(t, runner)
	resp := ts.doHeaders(t, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(slashChat, "L4", ""), map[string]string{FilterHeader: "provider=anthropic"})
	_ = resp.Body.Close()
	if n := len(runner.snapshot()); n != 0 {
		t.Fatalf("a request without slash= ran the tool command %d times", n)
	}
	if resp.Header.Get(ToolCommandHeader) != "" {
		t.Fatal("ordinary request carries the tool-command header")
	}
}

func TestResolveAgentRefusesSlash(t *testing.T) {
	ts := newSlashServer(t, &fakeRunner{})
	if _, err := ts.ResolveAgent(context.Background(), "L4", "slash=plan"); err == nil || !strings.Contains(err.Error(), "sticky") {
		t.Fatalf("ResolveAgent froze a slash filter: %v", err)
	}
}
