package cligw

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Declared effort reaches the CLI with the tool's own flag; undeclared, the
// argv is byte-for-byte what TestMeasuredWarmArgv pins.
func TestEffortArgv(t *testing.T) {
	claude := func(effort string) *Worker {
		return &Worker{mode: WarmStdinStreamJSON, launch: agentlaunch.Launch{Tool: "claude", Effort: effort, Args: []string{"--model", "M", "-p"}},
			tool: fleet.Tool{Name: "claude", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--output-format stream-json --verbose"}}}}
	}
	codex := func(effort string) *Worker {
		return &Worker{cwd: "/tmp/cligw-test", mode: WarmStdin, launch: agentlaunch.Launch{Tool: "codex", Effort: effort, Args: []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only"}},
			tool: fleet.Tool{Name: "codex", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--json"}}}}
	}
	claudeBase := claude("").argv("", "")
	wantClaude := append(append([]string{}, claudeBase...), "--effort", "high")
	if got := claude("high").argv("", ""); !reflect.DeepEqual(got, wantClaude) {
		t.Errorf("claude argv = %#v\nwant %#v", got, wantClaude)
	}
	codexBase := codex("").argv("", "")
	wantCodex := append([]string{"codex", "exec", "-c", `model_reasoning_effort="xhigh"`}, codexBase[2:]...)
	if got := codex("xhigh").argv("", ""); !reflect.DeepEqual(got, wantCodex) {
		t.Errorf("codex argv = %#v\nwant %#v", got, wantCodex)
	}
	for _, base := range [][]string{claudeBase, codexBase} {
		joined := strings.Join(base, " ")
		if strings.Contains(joined, "effort") {
			t.Errorf("undeclared effort leaked into argv: %s", joined)
		}
	}
}

func installEffortCatalog(t *testing.T, toolName, effort string) {
	t.Helper()
	t.Setenv("BASHY_SELF", filepath.Join(t.TempDir(), "no-such-bashy"))
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	launch := fleet.ToolLaunch{Exec: "/nonexistent/" + toolName + " -p {prompt}", Warm: string(WarmCold), EventsStdout: "--json"}
	if err := cat.SaveTool(fleet.Tool{Name: toolName, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "/nonexistent/" + toolName, Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "effort-agent", Tool: toolName, Effort: effort}); err != nil {
		t.Fatal(err)
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })
}

// The declared effort flows fleet agent -> launch -> worker argv (a cold
// worker starts no process until Do, so nothing is exec'd here).
func TestNewWorkerCarriesDeclaredEffort(t *testing.T) {
	installEffortCatalog(t, "claude", "max")
	w, err := NewWorker(context.Background(), "effort-agent")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got := strings.Join(w.argv("hi", ""), " ")
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("argv lacks --effort max: %s", got)
	}
}

// A tool with no known effort flag is refused loudly, never silently served
// at its default while the identity claims otherwise.
func TestNewWorkerRefusesEffortForUnsupportedTool(t *testing.T) {
	installEffortCatalog(t, "agy", "high")
	w, err := NewWorker(context.Background(), "effort-agent")
	if err == nil {
		w.Close()
		t.Fatal("agy with a declared effort was accepted")
	}
	if !strings.Contains(err.Error(), "no known effort flag") || !strings.Contains(err.Error(), `"agy"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestSaveAgentRejectsFlagShapedEffort(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	for _, bad := range []string{"high --x", "High", `high" -c x="y`} {
		if err := cat.SaveAgent(fleet.Agent{Name: "a", Tool: "claude", Effort: bad}); err == nil {
			t.Errorf("effort %q accepted", bad)
		}
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "a", Tool: "claude", Effort: "xhigh"}); err != nil {
		t.Errorf("xhigh refused: %v", err)
	}
}

// The catalog projection carries the effort the broker freezes and re-checks.
func TestCatalogAgentCarriesEffort(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	launch := fleet.ToolLaunch{Exec: "/nonexistent/claude -p {prompt}", Warm: string(WarmCold)}
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "/nonexistent/claude", Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "m1", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "claude-m1", Tool: "claude", Model: "m1", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	a, ok := NewFleetCatalog(cat).Agent("claude-m1")
	if !ok {
		t.Fatal("agent not projected")
	}
	if a.Effort != "high" {
		t.Fatalf("effort = %q", a.Effort)
	}
}

type effortCapture struct {
	Args        []string `json:"args"`
	GenieEffort string   `json:"genie_effort"`
}

func readEffortCapture(t *testing.T, path string) effortCapture {
	t.Helper()
	var c effortCapture
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &c) != nil {
		t.Fatalf("capture: %s: %v", data, err)
	}
	return c
}

func effortFlagOf(tool string, args []string) string {
	for i, a := range args {
		switch {
		case tool == "claude" && a == "--effort" && i+1 < len(args):
			return args[i+1]
		case tool == "codex" && a == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "model_reasoning_effort="):
			v, _ := strconv.Unquote(strings.TrimPrefix(args[i+1], "model_reasoning_effort="))
			return v
		}
	}
	return ""
}

// A request's own effort reaches the CLI on a cold worker and on a warm worker
// that was prewarmed without it (the warm one is relaunched).
func TestRequestEffortReachesArgv(t *testing.T) {
	for _, tt := range []struct {
		tool string
		warm WarmMode
	}{
		{"claude", WarmCold}, {"claude", WarmStdinStreamJSON},
		{"codex", WarmCold}, {"codex", WarmStdin},
	} {
		t.Run(tt.tool+"/"+string(tt.warm), func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
			installFakeCatalog(t, tt.tool, tt.warm, "system-prompt-"+tt.tool)
			w, err := NewWorker(context.Background(), "test-agent")
			if err != nil {
				t.Fatal(err)
			}
			got, err := w.DoCompletion(context.Background(), CompletionPrompt{Prompt: "hello", Effort: "xhigh"}, nil)
			if err != nil || got.Text != "ok" {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			if eff := effortFlagOf(tt.tool, readEffortCapture(t, capturePath).Args); eff != "xhigh" {
				t.Fatalf("effort in argv = %q, want xhigh", eff)
			}
		})
	}
}

// A request that names no effort leaves the argv exactly as before.
func TestRequestWithoutEffortKeepsLaunchEffort(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
	installFakeCatalogEffort(t, "claude", WarmStdinStreamJSON, "system-prompt-claude", "high")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.DoCompletion(context.Background(), CompletionPrompt{Prompt: "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if eff := effortFlagOf("claude", readEffortCapture(t, capturePath).Args); eff != "high" {
		t.Fatalf("effort = %q, want the binding's high", eff)
	}
}

// Naming the effort the binding already declares is not a conflict.
func TestRequestEffortMatchingBindingIsServed(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
	installFakeCatalogEffort(t, "codex", WarmStdin, "system-prompt-codex", "high")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.DoCompletion(context.Background(), CompletionPrompt{Prompt: "hello", Effort: "high"}, nil); err != nil {
		t.Fatal(err)
	}
	if eff := effortFlagOf("codex", readEffortCapture(t, capturePath).Args); eff != "high" {
		t.Fatalf("effort = %q", eff)
	}
}

// The sticky identity freezes the declared effort: a request for another is
// refused naming both, before a worker is spent on it.
func TestRequestEffortConflictingWithBindingIs400(t *testing.T) {
	installFakeCatalogEffort(t, "claude", WarmCold, "system-prompt-claude", "high")
	pool := NewPool(context.Background(), "test-agent", PoolConfig{StartServers: 1, MaxWorkers: 1, MaxSpare: 1})
	defer pool.Close()
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	backend := NewAgentBackend("test-agent", "test-model", pool)
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusBadRequest || attempt.CanRetry {
		t.Fatalf("attempt = %+v", attempt)
	}
	body := errorMessage(t, rec.Body.Bytes())
	if !strings.Contains(body, `"high"`) || !strings.Contains(body, `"low"`) {
		t.Fatalf("400 body does not name both efforts: %s", body)
	}
}

// A tool with no effort mechanism answers 400 to a request effort; it is never
// silently served at its default.
func TestRequestEffortUnsupportedToolIs400(t *testing.T) {
	installFakeCatalog(t, "agy", WarmCold, "system-prompt-agy")
	pool := NewPool(context.Background(), "test-agent", PoolConfig{StartServers: 1, MaxWorkers: 1, MaxSpare: 1})
	defer pool.Close()
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	backend := NewAgentBackend("test-agent", "test-model", pool)
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusBadRequest || attempt.CanRetry {
		t.Fatalf("attempt = %+v", attempt)
	}
	if body := errorMessage(t, rec.Body.Bytes()); !strings.Contains(body, "no known effort flag") || !strings.Contains(body, `"agy"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestRequestEffortInvalidIs400(t *testing.T) {
	backend, cleanup := testBackend(t, "backend-text")
	defer cleanup()
	for _, bad := range []string{"High", "high --x"} {
		_, attempt := serveBackend(t, backend, `{"model":"test-model","reasoning_effort":"`+bad+`","messages":[{"role":"user","content":"hi"}]}`, nil)
		if attempt.Status != http.StatusBadRequest {
			t.Errorf("effort %q: attempt = %+v", bad, attempt)
		}
	}
}

// A served request effort runs end to end through the door.
func TestRequestEffortServedThroughBackend(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
	installFakeCatalog(t, "claude", WarmCold, "system-prompt-claude")
	pool := NewPool(context.Background(), "test-agent", PoolConfig{StartServers: 1, MaxWorkers: 1, MaxSpare: 1})
	defer pool.Close()
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	backend := NewAgentBackend("test-agent", "test-model", pool)
	_, attempt := serveBackend(t, backend, `{"model":"test-model","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusOK {
		t.Fatalf("attempt = %+v", attempt)
	}
	if eff := effortFlagOf("claude", readEffortCapture(t, capturePath).Args); eff != "medium" {
		t.Fatalf("effort = %q", eff)
	}
}

// genie takes effort in its environment: from the binding at launch, and from
// the request on a worker that was prewarmed without one.
func TestGenieEffortEnvFromBindingAndRequest(t *testing.T) {
	for _, tt := range []struct {
		name, binding, request, want string
	}{
		{"binding", "high", "", "high"},
		{"request", "", "low", "low"},
		{"both-equal", "high", "high", "high"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
			installFakeCatalogEffort(t, "ycode", WarmStdin, "system-prompt-ycode", tt.binding)
			w, err := NewWorker(context.Background(), "test-agent")
			if err != nil {
				t.Fatalf("a genie binding's effort must not be refused: %v", err)
			}
			if _, err := w.DoCompletion(context.Background(), CompletionPrompt{Prompt: "hello", Effort: tt.request}, nil); err != nil {
				t.Fatal(err)
			}
			c := readEffortCapture(t, capturePath)
			if c.GenieEffort != tt.want {
				t.Fatalf("GENIE_EFFORT = %q, want %q", c.GenieEffort, tt.want)
			}
			for _, a := range c.Args {
				if strings.Contains(a, "effort") {
					t.Fatalf("genie got an effort flag: %q", c.Args)
				}
			}
		})
	}
}

func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %s: %v", body, err)
	}
	return e.Error.Message
}
