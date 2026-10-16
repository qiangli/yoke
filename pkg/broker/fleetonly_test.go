package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// leanStubEngine imitates an Engine.Start on a lean binary: the bashy
// engines stub refuses to run local inference and says so.
type leanStubEngine struct{}

func (leanStubEngine) Start(context.Context) (string, error) {
	return "", fmt.Errorf("bashy ollama: local inference requires the engines build (-tags bashy_engines,embed_runner): %w", ErrEngineNotInBuild)
}

func (leanStubEngine) Stop(context.Context) error { return nil }

// crashEngine imitates a real engine failure during start-up.
type crashEngine struct{ err error }

func (c crashEngine) Start(context.Context) (string, error) { return "", c.err }
func (crashEngine) Stop(context.Context) error              { return nil }

// TestLeanBuildDoorServesFleetAgents is the documented lean behaviour: with
// no local engine by build, the door still starts, its health is OK, local
// routes name the missing build, and agent routes are served.
func TestLeanBuildDoorServesFleetAgents(t *testing.T) {
	cli := &fakeCLI{}
	b, err := New(context.Background(), Options{
		Engine:      leanStubEngine{},
		CLI:         cli,
		Token:       testToken,
		MemoryBytes: 64 << 30,
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("door with unavailable-by-build engine must start: %v", err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	if b.local {
		t.Fatal("door must not claim a local engine in fleet-agents-only mode")
	}
	srv := httptest.NewServer(b)
	defer srv.Close()
	get := func(path string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(data, &out)
		return resp, out
	}

	resp, out := get("/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", resp.StatusCode)
	}
	if out["local_engine"] != false {
		t.Fatalf("health must report no local engine: %v", out)
	}

	resp, out = get("/api/tags")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("local route status: %d", resp.StatusCode)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "no local engine in this build") {
		t.Fatalf("local route must name the missing build, got %q", msg)
	}

	// An agent route goes to the fleet backend.
	h := &harness{t: t, b: b, cli: cli, server: srv}
	resp, out = h.do("POST", "/v1/chat/completions", chat("L4"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent route: %d %v", resp.StatusCode, out)
	}
	if resp.Header.Get(BackendHeader) != BackendCLI {
		t.Fatalf("backend header %q", resp.Header.Get(BackendHeader))
	}
}

// TestCrashingEngineStillFailsStart keeps the old contract for a real
// failure: anything that is not unavailable-by-build fails the start.
func TestCrashingEngineStillFailsStart(t *testing.T) {
	boom := errors.New("broker: engine exited during start-up (see the engine log)")
	_, err := New(context.Background(), Options{
		Engine:   crashEngine{err: boom},
		CLI:      &fakeCLI{},
		Token:    testToken,
		StateDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a crashing engine must still fail the start")
	}
	if EngineNotInBuild(err) {
		t.Fatalf("a crash must not classify as unavailable-by-build: %v", err)
	}
}

// TestExecEngineClassifiesLeanStub checks the child-process signal end to
// end: the stub's line means unavailable-by-build, a bare crash does not.
func TestExecEngineClassifiesLeanStub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to imitate the engine child")
	}
	ctx := context.Background()

	stub := &ExecEngine{Argv: []string{"sh", "-c", `echo "bashy ollama: local inference requires the engines build (-tags bashy_engines,embed_runner)" >&2; exit 1`}}
	_, err := stub.Start(ctx)
	if !EngineNotInBuild(err) {
		t.Fatalf("stub exit must classify as unavailable-by-build, got %v", err)
	}

	var log bytes.Buffer
	crash := &ExecEngine{Argv: []string{"sh", "-c", "exit 1"}, Log: &log}
	_, err = crash.Start(ctx)
	if err == nil || EngineNotInBuild(err) {
		t.Fatalf("bare crash must fail as before, got %v", err)
	}
	if !strings.Contains(err.Error(), "exited during start-up") {
		t.Fatalf("crash keeps the old error, got %v", err)
	}
}
