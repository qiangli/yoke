package broker

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/fleet"
)

type reloadQuota struct{}

func (reloadQuota) Headroom(context.Context, cligw.Agent) (float64, bool) { return 1, true }
func (reloadQuota) Preview(context.Context, cligw.Agent) (bool, string)   { return true, "" }

func TestDoorReloadCatalogAndSticky(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(fleet.Tool{Name: "reload-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: binary, Launch: fleet.ToolLaunch{Exec: binary + " {prompt}"}}}); err != nil {
		t.Fatal(err)
	}
	save := func(name string) {
		t.Helper()
		if err := cat.SaveModel(fleet.Model{Name: name, Kind: fleet.ModelKindSubscription, Provider: "test", Band: 2}); err != nil {
			t.Fatal(err)
		}
		if err := cat.SaveAgent(fleet.Agent{Name: name + "-agent", Tool: "reload-tool", Model: name}); err != nil {
			t.Fatal(err)
		}
	}
	save("original")
	cli, err := cligw.NewServer(cligw.ServerOptions{Catalog: cligw.NewFleetCatalog(cat), Token: "test", Quota: reloadQuota{}, Pool: cligw.PoolConfig{MaxWorkers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	h := newHarness(t, func(o *Options) { o.CLI = cliAdapter{s: cli}; o.ToolVersion = nil })
	resp, out := h.do("POST", "/v1/sticky", StickySpec{Key: "stable", Model: "original-agent"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create sticky: %d %v", resp.StatusCode, out)
	}
	digest := out["digest"]
	list := func() map[string]bool {
		t.Helper()
		resp, out := h.do("GET", "/v1/models", nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("list: %d %v", resp.StatusCode, out)
		}
		names := map[string]bool{}
		for _, row := range out["data"].([]any) {
			names[row.(map[string]any)["id"].(string)] = true
		}
		return names
	}
	if !list()["original-agent"] {
		t.Fatal("initial agent missing")
	}
	// Exercise simultaneous listings and identity lookups across real TTL reloads.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(20 * time.Millisecond):
					cli.Agent("original-agent")
					list()
					h.b.refreshTags(ctx, false)
				}
			}
		}()
	}
	defer func() { cancel(); wg.Wait() }()
	wait := func(predicate func(map[string]bool) bool) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			if predicate(list()) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("catalog did not reload within two polling intervals")
	}
	save("added")
	h.eng.mu.Lock()
	h.eng.models["pulled:latest"] = engineModel{Name: "pulled:latest", Digest: "new"}
	h.eng.mu.Unlock()
	wait(func(names map[string]bool) bool {
		return names["added"] && names["added-agent"] && names["pulled:latest"]
	})
	resp, out = h.do("POST", "/v1/sticky", StickySpec{Key: "stable", Model: "original-agent"}, nil)
	if resp.StatusCode != 200 || out["digest"] != digest {
		t.Fatalf("unchanged sticky lost: %d %v", resp.StatusCode, out)
	}
	if err := cat.RemoveAgent("original-agent"); err != nil {
		t.Fatal(err)
	}
	if err := cat.RemoveModel("original"); err != nil {
		t.Fatal(err)
	}
	h.eng.mu.Lock()
	delete(h.eng.models, "pulled:latest")
	h.eng.mu.Unlock()
	wait(func(names map[string]bool) bool {
		return !names["original"] && !names["original-agent"] && !names["pulled:latest"]
	})
	// Force an explicit lookup to await the catalog projection (the list also
	// excludes a dangling live binding before its cached projection expires).
	deadline := time.Now().Add(12 * time.Second)
	for {
		_, ok := cli.Agent("original-agent")
		if !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removed agent cached")
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, out = h.do(http.MethodPost, "/sticky/stable/v1/chat/completions", chat("added"), nil)
	if resp.StatusCode != 409 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "disappeared") {
		t.Fatalf("removed identity: %d %v", resp.StatusCode, out)
	}
}
