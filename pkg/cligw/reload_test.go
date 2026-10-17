package cligw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

func TestReloadReplacesChangedPoolPreservesUnchanged(t *testing.T) {
	cat := testFleet(t)
	s, err := NewServer(ServerOptions{Catalog: cat, Token: "test", Pool: PoolConfig{MaxWorkers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	alpha, err := s.Backend("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := s.Backend("beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Registry().SaveAgent(fleet.Agent{Name: "alpha", Tool: "alpha-tool", Model: "frontier"}); err != nil {
		t.Fatal(err)
	}
	cat.mu.Lock()
	cat.cachedAt = time.Time{}
	cat.mu.Unlock()
	next, err := s.Backend("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if next == alpha {
		t.Fatal("changed identity retained its old pool")
	}
	same, err := s.Backend("beta")
	if err != nil || same != beta {
		t.Fatalf("unchanged identity lost pool: %v", err)
	}
}

func TestReloadRemovedAgentKeepsInFlightWorker(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	registry := agentlaunch.NewCatalog()
	tool, _ := registry.Tool("claude")
	tool.CLI.Launch.Exec = strings.Replace(tool.CLI.Launch.Exec, "{prompt}", "--model {model} {prompt}", 1)
	if err := registry.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	if err := registry.SaveModel(fleet.Model{Name: "fixture", Kind: fleet.ModelKindSubscription}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SaveAgent(fleet.Agent{Name: "test-agent", Tool: "claude", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := agentlaunch.Resolve("test-agent", agentlaunch.Options{ReadOnly: true, Sandbox: "read-only"}); err != nil {
		t.Fatal(err)
	}
	cat := NewFleetCatalog(registry)
	s, err := NewServer(ServerOptions{Catalog: cat, Token: "test", Pool: PoolConfig{MaxWorkers: 1, MaxSpare: 1}, Quota: &fakeQuota{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backend, err := s.Backend("test-agent")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	worker, err := backend.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v, stats %+v", err, backend.Pool.Stats())
	}
	defer backend.Pool.Release(worker)
	if err := registry.RemoveAgent("test-agent"); err != nil {
		t.Fatal(err)
	}
	cat.mu.Lock()
	cat.cachedAt = time.Time{}
	cat.mu.Unlock()
	if _, err := s.Backend("test-agent"); err == nil {
		t.Fatal("removed agent still has a routable pool")
	}
	if _, err := worker.Do(ctx, "already admitted", nil); err != nil {
		t.Fatalf("reload killed admitted worker: %v", err)
	}
}
