package cligw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// The server tests run against FAKE CLIs — this test binary re-executed
// through TestCLIHelper — so nothing here spawns claude, codex or agy, and
// every path (token, policy, usage.jsonl, endpoint.json) is under a
// per-test BASHY_HOME.

// installServerFleet builds a two-agent L4 band plus one L2 agent over the
// fake CLI helper: "warm-four" takes its prompt as stream-json on stdin (the
// measured claude contract) and "cold-four" takes it on argv.
func installServerFleet(t *testing.T) *FleetCatalog {
	t.Helper()
	t.Setenv("CLIGW_TEST_HELPER", "1")
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())

	launch := func(mode string, warm WarmMode) fleet.ToolLaunch {
		return fleet.ToolLaunch{
			// {model} rides AFTER the -- separator: a --model flag before it
			// would be parsed by the child test binary's own flag set and
			// rejected before the helper ever runs.
			Exec:         fmt.Sprintf("%s -test.run=TestCLIHelper -- %s {model} {prompt}", os.Args[0], mode),
			Warm:         string(warm),
			EventsStdout: "--events-json",
			EventsDone:   fleet.EventsDone{Field: "type", Values: []string{"result", "turn.completed"}},
		}
	}
	tools := []fleet.Tool{
		{Name: "claude", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch("warm", WarmStdinStreamJSON)}},
		{Name: "fakecold", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch("cold", WarmCold)}},
	}
	for _, tool := range tools {
		if err := cat.SaveTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	models := []fleet.Model{
		{Name: "sonnet-x", Band: 4, BandSource: fleet.BandMeasured, Kind: fleet.ModelKindSubscription, Provider: "anthropic", Domain: []string{"coding"}},
		{Name: "gpt-x", Band: 4, BandSource: fleet.BandMeasured, Kind: fleet.ModelKindSubscription, Provider: "openai", Domain: []string{"coding"}},
		{Name: "tiny", Band: 2, Kind: fleet.ModelKindLocal, Provider: "local", Domain: []string{"general"}},
	}
	for _, model := range models {
		if err := cat.SaveModel(model); err != nil {
			t.Fatal(err)
		}
	}
	agents := []fleet.Agent{
		{Name: "warm-four", Tool: "claude", Model: "sonnet-x"},
		{Name: "cold-four", Tool: "fakecold", Model: "gpt-x"},
		{Name: "small-two", Tool: "claude", Model: "tiny"},
	}
	for _, agent := range agents {
		if err := cat.SaveAgent(agent); err != nil {
			t.Fatal(err)
		}
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })
	return NewFleetCatalog(cat)
}

type testServer struct {
	*Server
	http  *httptest.Server
	quota *fakeQuota
}

func (ts *testServer) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.http.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func newTestServer(t *testing.T, headroom map[string]float64) *testServer {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	catalog := installServerFleet(t)
	quota := &fakeQuota{headroom: headroom, refused: map[string]string{}}
	server, err := NewServer(ServerOptions{
		Catalog: catalog,
		Quota:   quota,
		Breaker: sched.NewBreaker(),
		Pool:    PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 1, MaxWorkers: 2, IdleTTL: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &testServer{Server: server, http: httpServer, quota: quota}
}

const chatBody = `{"model":%q,"messages":[{"role":"user","content":"say ok"}]}`

func TestServerModelListCoversBandsAndAgents(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	resp := ts.do(t, http.MethodGet, "/v1/models", ts.Token(), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models = %s", resp.Status)
	}
	var list ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]ModelEntry, len(list.Data))
	for _, entry := range list.Data {
		byID[entry.ID] = entry
	}
	for _, want := range []string{"L1", "L2", "L3", "L4", "L5", "L4+", "warm-four", "cold-four", "small-two", "sonnet-x", "gpt-x", "tiny"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("/v1/models is missing %q: %v", want, list.Data)
		}
	}
	if got := byID["warm-four"]; got.XBand != 4 || got.XTool != "claude" || got.XWarm != string(WarmStdinStreamJSON) {
		t.Fatalf("warm-four entry = %+v", got)
	}
	if got := byID["sonnet-x"]; got.XQuota != "0.80" {
		t.Fatalf("x_quota = %q, want the stubbed headroom", got.XQuota)
	}
	if got := byID["L4"]; got.XQuota != "" {
		t.Fatalf("a band alias spans many seats and cannot carry one headroom: %+v", got)
	}

	// The alias mount is the second spelling of the same route.
	alias := ts.do(t, http.MethodGet, "/openai/v1/models", ts.Token(), "")
	defer alias.Body.Close()
	if alias.StatusCode != http.StatusOK {
		t.Fatalf("GET /openai/v1/models = %s", alias.Status)
	}
}

// TestServerModelListExcludesUnservableModels pins S406/#1772: a model row
// with no installed tool, or whose only agent is in breaker cooldown, must
// not appear in /v1/models — a listed model must never be a request that is
// guaranteed to hit the router's 503.
func TestServerModelListExcludesUnservableModels(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("CLIGW_TEST_HELPER", "1")
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())

	tools := []fleet.Tool{
		{Name: "claude", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{
			Binary: os.Args[0],
			Launch: fleet.ToolLaunch{
				Exec:         fmt.Sprintf("%s -test.run=TestCLIHelper -- warm {model} {prompt}", os.Args[0]),
				EventsStdout: "--events-json",
				EventsDone:   fleet.EventsDone{Field: "type", Values: []string{"result", "turn.completed"}},
			},
		}},
		{Name: "ghost-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{
			Binary: filepath.Join(root, "does-not-exist"),
			Launch: fleet.ToolLaunch{Exec: "does-not-exist {model} {prompt}"},
		}},
	}
	for _, tool := range tools {
		if err := cat.SaveTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	models := []fleet.Model{
		{Name: "sonnet-x", Band: 4, Kind: fleet.ModelKindSubscription, Provider: "anthropic", Domain: []string{"coding"}},
		{Name: "ghost-model", Band: 4, Kind: fleet.ModelKindSubscription, Provider: "ghost", Domain: []string{"coding"}},
		{Name: "frozen-model", Band: 4, Kind: fleet.ModelKindSubscription, Provider: "frozen", Domain: []string{"coding"}},
	}
	for _, model := range models {
		if err := cat.SaveModel(model); err != nil {
			t.Fatal(err)
		}
	}
	agents := []fleet.Agent{
		{Name: "warm-four", Tool: "claude", Model: "sonnet-x"},
		{Name: "ghost-agent", Tool: "ghost-tool", Model: "ghost-model"},
		{Name: "frozen-agent", Tool: "claude", Model: "frozen-model"},
	}
	for _, agent := range agents {
		if err := cat.SaveAgent(agent); err != nil {
			t.Fatal(err)
		}
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })

	catalog := NewFleetCatalog(cat)
	quota := &fakeQuota{headroom: map[string]float64{}, refused: map[string]string{}}
	breaker := sched.NewBreaker()
	server, err := NewServer(ServerOptions{
		Catalog: catalog, Quota: quota, Breaker: breaker,
		Pool: PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 1, MaxWorkers: 2, IdleTTL: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	breaker.Trip("frozen-agent", time.Minute)

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	ts := &testServer{Server: server, http: httpServer, quota: quota}

	resp := ts.do(t, http.MethodGet, "/v1/models", ts.Token(), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models = %s", resp.Status)
	}
	var list ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]ModelEntry, len(list.Data))
	for _, entry := range list.Data {
		byID[entry.ID] = entry
	}
	for _, want := range []string{"warm-four", "sonnet-x"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("servable row %q is missing from /v1/models: %v", want, list.Data)
		}
	}
	for _, unwanted := range []string{"ghost-agent", "ghost-model", "frozen-agent", "frozen-model"} {
		if _, ok := byID[unwanted]; ok {
			t.Fatalf("/v1/models lists %q, which cannot currently be served: %v", unwanted, list.Data)
		}
	}
}

func TestServerRejectsBadToken(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		method, body := http.MethodGet, ""
		if path == "/v1/chat/completions" {
			method, body = http.MethodPost, fmt.Sprintf(chatBody, "L4")
		}
		resp := ts.do(t, method, path, "not-the-token", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s with a bad token = %s, want 401", method, path, resp.Status)
		}
		_ = resp.Body.Close()

		missing := ts.do(t, method, path, "", body)
		if missing.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s with no token = %s, want 401", method, path, missing.Status)
		}
		_ = missing.Body.Close()
	}
	// A rejected request must not have created a pool or spawned anything.
	if pools := ts.Health().Autoscale.Pools; len(pools) != 0 {
		t.Fatalf("unauthorized requests created pools: %+v", pools)
	}
}

func TestServerRoutesBandByHeadroomAndStampsRoutedHeader(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	resp := ts.do(t, http.MethodGet, "/v1/models", ts.Token(), "")
	_ = resp.Body.Close()

	first := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "L4"))
	body, _ := io.ReadAll(first.Body)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("chat = %s: %s", first.Status, body)
	}
	if got := first.Header.Get(RoutedHeader); !strings.HasPrefix(got, "agent=warm-four, band=L4, reason=quota-first") {
		t.Fatalf("%s = %q, want the quota leader", RoutedHeader, got)
	}
	if got := ts.quota.headrooms("sonnet-x"); got != 1 {
		t.Fatalf("sonnet-x headroom reads after /v1/models and Route = %d, want one shared cached read", got)
	}
	if got := ts.quota.headrooms("gpt-x"); got != 1 {
		t.Fatalf("gpt-x headroom reads after /v1/models and Route = %d, want one shared cached read", got)
	}
	var completion struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Model != "L4" || len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, "answer:") {
		t.Fatalf("completion = %s", body)
	}

	// Flip the headroom: the SAME request must now land on the other L4
	// agent, which is the whole point of routing through cligw's Router
	// rather than the gateway's own least-loaded pick.
	ts.quota.headroom["sonnet-x"], ts.quota.headroom["gpt-x"] = .1, .9
	expireHeadroom(ts.Server.quota)
	second := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "L4"))
	secondBody, _ := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second chat = %s: %s", second.Status, secondBody)
	}
	if got := second.Header.Get(RoutedHeader); !strings.HasPrefix(got, "agent=cold-four, band=L4") {
		t.Fatalf("%s = %q, want the new quota leader", RoutedHeader, got)
	}

	// A client can consume the declared Content-Length before the proxy's
	// audit-tail cleanup finishes. Wait for handlers to return before checking
	// both accounting records; Close is also safe for the registered cleanup.
	ts.http.Close()
	// Both the decision and the served usage are in usage.jsonl.
	lines := readUsageLog(t)
	var decisions, usage int
	for _, line := range lines {
		if strings.Contains(line, `"ranked"`) {
			decisions++
		}
		if strings.Contains(line, UsageSchemaVersion) {
			usage++
		}
	}
	if decisions != 2 || usage != 2 {
		t.Fatalf("usage.jsonl decisions=%d usage=%d in %v", decisions, usage, lines)
	}
}

func TestServerPinnedAgentAndUnknownModel(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	pinned := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "cold-four"))
	body, _ := io.ReadAll(pinned.Body)
	_ = pinned.Body.Close()
	if pinned.StatusCode != http.StatusOK {
		t.Fatalf("pinned agent = %s: %s", pinned.Status, body)
	}
	if got := pinned.Header.Get(RoutedHeader); !strings.HasPrefix(got, "agent=cold-four") {
		t.Fatalf("%s = %q, want the pinned agent even with less headroom", RoutedHeader, got)
	}

	unknown := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "sonnet-y"))
	unknownBody, _ := io.ReadAll(unknown.Body)
	_ = unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model = %s, want 404: %s", unknown.Status, unknownBody)
	}
	if !strings.Contains(string(unknownBody), "did you mean") {
		t.Fatalf("404 body carries no suggestions: %s", unknownBody)
	}
}

func TestServerReserveFloorAnswers429(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .05, "gpt-x": .05})

	resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "L4"))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exhausted band = %s, want 429: %s", resp.Status, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if !strings.Contains(string(body), "reserve floor") {
		t.Fatalf("429 body does not say why: %s", body)
	}
}

func TestServerEmbeddingsAreRefusedNotRouted(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8})
	resp := ts.do(t, http.MethodPost, "/v1/embeddings", ts.Token(), `{"model":"L4","input":"hello"}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "completion surface") {
		t.Fatalf("embeddings = %s: %s", resp.Status, body)
	}
}

func TestServerHealthCarriesPoolsAndAutoscaleSnapshot(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "L4"))
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	health := ts.do(t, http.MethodGet, "/health", "", "")
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %s", health.Status)
	}
	var report HealthReport
	if err := json.NewDecoder(health.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != HealthSchemaVersion || report.Status != "ok" {
		t.Fatalf("health envelope = %+v", report)
	}
	if len(report.Agents) != 1 || report.Agents[0].Agent != "warm-four" || report.Agents[0].Band != 4 {
		t.Fatalf("health agents = %+v", report.Agents)
	}
	if report.Autoscale.SchemaVersion != AutoscaleSchemaVersion || len(report.Autoscale.Pools) != 1 {
		t.Fatalf("autoscale snapshot = %+v", report.Autoscale)
	}
	if pool := report.Autoscale.Pools[0]; pool.Agent != "warm-four" || pool.Band != 4 || pool.MaxWorkers != 2 {
		t.Fatalf("pool snapshot = %+v", pool)
	}
	if report.Policy.Default != PolicyQuotaFirst || report.Policy.ReserveFloor != DefaultPolicy().ReserveFloor {
		t.Fatalf("policy health = %+v", report.Policy)
	}

	// A tripped breaker is what "degraded" means: every pool cooling.
	ts.breaker.Trip("warm-four", time.Minute)
	degraded := ts.do(t, http.MethodGet, "/health", "", "")
	defer degraded.Body.Close()
	var second HealthReport
	if err := json.NewDecoder(degraded.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.Status != "degraded" || !second.Agents[0].Cooling {
		t.Fatalf("degraded health = %+v", second)
	}
}

func TestServerFilterHeaderNarrowsCandidates(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	req, err := http.NewRequest(http.MethodPost, ts.http.URL+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(chatBody, "L4")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.Token())
	req.Header.Set(FilterHeader, "provider=openai")
	resp, err := ts.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered chat = %s: %s", resp.Status, body)
	}
	if got := resp.Header.Get(RoutedHeader); !strings.HasPrefix(got, "agent=cold-four") {
		t.Fatalf("%s = %q, want the only openai candidate", RoutedHeader, got)
	}
}

func TestServerPrewarmCreatesTheBandLeaders(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2, "tiny": .5})
	warmed := ts.Prewarm()
	if len(warmed) != 2 {
		t.Fatalf("prewarmed %v, want one leader per populated band", warmed)
	}
	agents := map[string]bool{}
	for _, name := range warmed {
		agents[name] = true
	}
	if !agents["warm-four"] || !agents["small-two"] {
		t.Fatalf("prewarmed %v, want the L4 quota leader and the L2 agent", warmed)
	}
	if pools := ts.Health().Autoscale.Pools; len(pools) != 2 {
		t.Fatalf("pools = %+v", pools)
	}
}

func TestTokenFileIsOwnerOnlyAndStable(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	first, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("token = %q, want 32 random bytes hex-encoded", first)
	}
	path, err := TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", info.Mode().Perm())
	}
	second, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("a second serve minted a new token and invalidated every client")
	}
}

func TestUnixSocketBindIsOwnerOnlyAndServes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-socket permissions are a POSIX contract")
	}
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8})
	// A unix path has ~104 bytes of room, and t.TempDir() names are long
	// enough on macOS to blow it — hence the short name in the temp root.
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("cligw-%d.sock", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(socket) })

	listener, endpoint, err := Listen(BindUnixPrefix+socket, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", info.Mode().Perm())
	}
	if endpoint.Socket != socket || endpoint.Bind != BindUnixPrefix+socket {
		t.Fatalf("endpoint = %+v", endpoint)
	}

	srv := &http.Server{Handler: ts.Handler()}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()

	report, err := FetchHealth(context.Background(), endpoint, ts.Token())
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != HealthSchemaVersion {
		t.Fatalf("health over the socket = %+v", report)
	}

	// A live socket must never be clobbered by a second serve.
	if _, _, err := Listen(BindUnixPrefix+socket, 0); err == nil {
		t.Fatal("a second bind stole a socket that was already being served")
	}
}

func TestEndpointRecordAndEnvURLs(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	fallback, err := ReadEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Port != DefaultPort || fallback.OpenAIBaseURL() != fmt.Sprintf("http://127.0.0.1:%d/v1", DefaultPort) {
		t.Fatalf("default endpoint = %+v", fallback)
	}
	if fallback.AnthropicBaseURL() != fmt.Sprintf("http://127.0.0.1:%d/anthropic", DefaultPort) {
		t.Fatalf("anthropic base = %q", fallback.AnthropicBaseURL())
	}

	written := Endpoint{SchemaVersion: EndpointSchemaVersion, BaseURL: "http://127.0.0.1:1", Bind: BindLoopback, Port: 1}
	if err := WriteEndpoint(written); err != nil {
		t.Fatal(err)
	}
	path, err := EndpointPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("endpoint mode = %v, want 0600", info.Mode().Perm())
	}
	got, err := ReadEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != written.BaseURL || got.Port != 1 {
		t.Fatalf("endpoint round trip = %+v", got)
	}
	if got.AnthropicBaseURL() != "" {
		t.Fatal("an endpoint that did not advertise /anthropic must not print ANTHROPIC_BASE_URL")
	}
	if err := RemoveEndpoint(); err != nil {
		t.Fatal(err)
	}
	if err := RemoveEndpoint(); err != nil {
		t.Fatalf("removing an absent endpoint must be a no-op: %v", err)
	}
}

func TestLoopbackIsTheDefaultAndLANIsExplicit(t *testing.T) {
	listener, endpoint, err := listen("", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if endpoint.Bind != BindLoopback || !strings.HasPrefix(endpoint.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("default bind = %+v", endpoint)
	}
	if addr := listener.Addr().String(); !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("default listener bound %s, want loopback only", addr)
	}
	if _, _, err := Listen("everywhere", 0); err == nil {
		t.Fatal("an unknown bind mode must fail loudly")
	}
	if _, _, err := Listen(BindUnixPrefix, 0); err == nil {
		t.Fatal("unix: with no path must fail loudly")
	}
}

func readUsageLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(usagePath())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestServerStreamsSSEChunks(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(),
		`{"model":"L4","stream":true,"messages":[{"role":"user","content":"say ok"}]}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %s: %s", resp.Status, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an SSE stream", ct)
	}
	text := string(body)
	if !strings.Contains(text, `"chat.completion.chunk"`) || !strings.HasSuffix(strings.TrimSpace(text), "data: [DONE]") {
		t.Fatalf("stream body = %s", text)
	}
	if got := resp.Header.Get(RoutedHeader); !strings.HasPrefix(got, "agent=warm-four") {
		t.Fatalf("%s = %q", RoutedHeader, got)
	}
}

func TestServerAutoSelectorIsClassifiedBeforeRouting(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2, "tiny": .9})

	resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "auto"))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auto = %s: %s", resp.Status, body)
	}
	// "say ok" is the cheapest possible turn, so the classifier picks the
	// lowest band the fleet has and the router stays inside it: auto must
	// never be an excuse to spend an L4 seat on a greeting.
	routed := resp.Header.Get(RoutedHeader)
	if !strings.Contains(routed, "agent=small-two") || !strings.Contains(routed, "band=L2") {
		t.Fatalf("%s = %q, want the classified band", RoutedHeader, routed)
	}

	// The band it ran at is remembered, and the next identical prompt takes
	// the same one from history rather than re-deriving it.
	if n := ts.history.Len(); n != 1 {
		t.Fatalf("classified band was not recorded: %d history entries", n)
	}
	again := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "auto"))
	_, _ = io.ReadAll(again.Body)
	_ = again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("second auto = %s", again.Status)
	}
	if got := again.Header.Get(RoutedHeader); !strings.Contains(got, "band=L2") {
		t.Fatalf("%s = %q, want the remembered band", RoutedHeader, got)
	}
}

func TestServerPolicyHeaderSelectsAndValidates(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	post := func(policy string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.http.URL+"/v1/chat/completions",
			strings.NewReader(fmt.Sprintf(chatBody, "L4")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+ts.Token())
		req.Header.Set(PolicyHeader, policy)
		resp, err := ts.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// latency-first: neither pool is warm yet, so it falls through to the
	// same quota ordering but must SAY it used the latency policy.
	ok := post(PolicyLatencyFirst)
	body, _ := io.ReadAll(ok.Body)
	_ = ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("latency-first = %s: %s", ok.Status, body)
	}
	if got := ok.Header.Get(RoutedHeader); !strings.Contains(got, "reason=latency-first") {
		t.Fatalf("%s = %q", RoutedHeader, got)
	}

	bad := post("cheapest")
	badBody, _ := io.ReadAll(bad.Body)
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown policy = %s, want 400: %s", bad.Status, badBody)
	}
	if !strings.Contains(string(badBody), PolicyRoundRobin) || !strings.Contains(string(badBody), "prefer:") {
		t.Fatalf("400 does not list the policies: %s", badBody)
	}
}

func TestServerRankIsRestrictedToLivePools(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	// No pool yet: there is nowhere to place a spare, so the autoscaler's
	// Ranker must say nothing rather than pay for a full-band quota preview.
	if got := ts.Rank(4); got != nil {
		t.Fatalf("Rank(4) with no pool = %v, want nil", got)
	}
	if _, err := ts.Backend("cold-four"); err != nil {
		t.Fatal(err)
	}
	if got := ts.Rank(4); len(got) != 1 || got[0] != "cold-four" {
		t.Fatalf("Rank(4) = %v, want only the agent that has a pool", got)
	}
	// The unrestricted rank still sees the whole band — it is what decides
	// WHICH pool to create.
	full := ts.Router().Rank(context.Background(), 4, Filter{}, "")
	if len(full) != 2 || full[0] != "warm-four" {
		t.Fatalf("unrestricted Rank(4) = %v, want both L4 agents quota-first", full)
	}
	if got := ts.Rank(2); got != nil {
		t.Fatalf("Rank(2) = %v, want nil: no L2 pool exists", got)
	}
}
